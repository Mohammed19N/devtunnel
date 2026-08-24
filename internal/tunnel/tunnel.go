package tunnel

import (
	"fmt"
	"net"
)

func StartServer(listenAddress string) {
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		return
	}
	connection, err := listener.Accept()
	if err != nil {
		return
	}
	defer connection.Close()
	buffer := make([]byte, 4096)
	numOfBytes, err := connection.Read(buffer)
	if err != nil {
		return
	}
	fmt.Println(string(buffer[0:numOfBytes]))
}
