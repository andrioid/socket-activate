package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/godbus/dbus"
)

var (
	mode               = flag.String("m", "tcp", "mode, available: tcp")
	targetUnit         = flag.String("u", "null.service", "corresponding unit")
	destinationAddress = flag.String("a", "127.0.0.1:80", "destination address")
	timeout            = flag.Duration("t", 0, "inactivity timeout after which to stop the unit again")
	user               = flag.Bool("user", false, "run as user session")
	backendTimeout     = flag.Duration("backend-timeout", 30*time.Second, "maximum time to wait for backend connection")
)

type unitController struct {
	conn     *dbus.Conn
	unitname string
}

func newUnitController(name string) unitController {
	// Connect to SystemBus if user is false, otherwise connect to SessionBus
	if *user {
		conn, err := dbus.SessionBus()
		if err != nil {
			log.Fatal(err)
		}
		return unitController{conn, name}
	}
	// Connect to SystemBus
	conn, err := dbus.SystemBus()
	if err != nil {
		log.Fatal(err)
	}
	return unitController{conn, name}
}

func (unitCtrl unitController) startSystemdUnit() {
	var responseObjPath dbus.ObjectPath
	obj := unitCtrl.conn.Object("org.freedesktop.systemd1", dbus.ObjectPath("/org/freedesktop/systemd1"))
	err := obj.Call("org.freedesktop.systemd1.Manager.StartUnit", 0, unitCtrl.unitname, "replace").Store(&responseObjPath)
	if err != nil {
		log.Fatal(err)
	}

}

func (unitCtrl unitController) stopSystemdUnit() {
	var responseObjPath dbus.ObjectPath
	obj := unitCtrl.conn.Object("org.freedesktop.systemd1", dbus.ObjectPath("/org/freedesktop/systemd1"))
	err := obj.Call("org.freedesktop.systemd1.Manager.StopUnit", 0, unitCtrl.unitname, "replace").Store(&responseObjPath)
	if err != nil {
		log.Fatal(err)
	}

}

func (unitCtrl unitController) terminateWithoutActivity(activity <-chan bool) {
	for {
		select {
		case <-activity:
		case <-time.After(*timeout):
			unitCtrl.stopSystemdUnit()
			os.Exit(0)
		}
	}
}

func proxyNetworkConnections(from net.Conn, to net.Conn, activityMonitor chan<- bool) {
	buffer := make([]byte, 1024)

	for {
		i, err := from.Read(buffer)
		if err != nil {
			return // EOF (if anything else, we scrap the connection anyways)
		}
		activityMonitor <- true
		to.Write(buffer[:i])
	}
}

func startTCPProxy(activityMonitor chan<- bool) {
	l, err := net.FileListener(os.NewFile(3, "systemd-socket"))
	if err != nil {
		log.Fatal(err)
	}
	defer l.Close()

	var hadSuccessfulConnection bool
	startTime := time.Now()

	for {
		activityMonitor <- true
		connOutwards, err := l.Accept()
		if err != nil {
			fmt.Println(err)
			return
		}

		var connBackend net.Conn
		attempt := 0
		maxRetries := 10

		for {
			connBackend, err = net.Dial("tcp", *destinationAddress)
			if err == nil {
				break // Successfully connected
			}

			// If we had a successful connection before and now can't connect, exit
			if hadSuccessfulConnection {
				fmt.Println("Backend connection failed after previous success, exiting")
				os.Exit(0)
			}

			// Check if we've exceeded the backend timeout
			if time.Since(startTime) > *backendTimeout {
				fmt.Printf("Backend connection attempts exceeded timeout of %v, exiting\n", *backendTimeout)
				os.Exit(0)
			}

			attempt++
			if attempt >= maxRetries {
				attempt = maxRetries - 1 // Cap the delay at max retry level
			}

			// Calculate delay using exponential backoff
			delay := time.Duration(500*(1<<attempt)) * time.Millisecond
			fmt.Printf("Connection attempt failed, retrying in %v: %v\n", delay, err)
			time.Sleep(delay)
		}

		// Mark that we've had at least one successful connection
		hadSuccessfulConnection = true

		go proxyNetworkConnections(connOutwards, connBackend, activityMonitor)
		go proxyNetworkConnections(connBackend, connOutwards, activityMonitor)
	}
}

func main() {

	flag.Parse()
	if os.Getenv("LISTEN_PID") != strconv.Itoa(os.Getpid()) {
		log.Fatal("socket-activate is only meant to be run from a systemd unit, aborting.")
	}

	unitCtrl := newUnitController(*targetUnit)

	activityMonitor := make(chan bool)
	if *timeout != 0 {
		go unitCtrl.terminateWithoutActivity(activityMonitor)
	}

	// first, connect to systemd for starting the unit
	unitCtrl.startSystemdUnit()

	// then take over the socket from systemd
	startTCPProxy(activityMonitor)
}
