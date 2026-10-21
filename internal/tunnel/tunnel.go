package tunnel

import (
	"net"
)

func StartServer(listenAddress string) {
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		return
	}
	for {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		go HandleConnection(connection)
	}
}

func HandleConnection(connection net.Conn) {
	defer connection.Close()
	buffer := make([]byte, 4096)
	for {
		numOfBytes, err := connection.Read(buffer)
		if err != nil {
			break
		}
		_, writeErr := connection.Write(buffer[0:numOfBytes])
		if writeErr != nil {
			return
		}
	}
}
