package main

import (
	"fmt"
	"net"
	"os"
)

// nolint:cyclop
func main() {
	c := make(chan []byte, 100)
	d := make(chan []byte, 100)

	go runPeerConnection(c, d)

	// Resolve the string address to a UDP address
	udpAddr, err := net.ResolveUDPAddr("udp", ":27015")

	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	// Start listening for UDP packages on the given address
	conn, err := net.ListenUDP("udp", udpAddr)

	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	go func() {
		for range d {
			fmt.Println("Received message")
		}
	}()

	// Read from UDP listener in endless loop
	for {
		var buf [512]byte
		_, _, err := conn.ReadFromUDP(buf[0:])
		if err != nil {
			fmt.Println(err)
			return
		}
		c <- buf[:]
	}
}
