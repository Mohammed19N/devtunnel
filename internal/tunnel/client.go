package tunnel

import (
	"fmt"
	"net"
)

func StartClient(serverAddress string) {
	connection, err := net.Dial("tcp", serverAddress)
	if err != nil {
		return
	}
	defer connection.Close()
	msg1 := []byte("hello")
	_, err1 := connection.Write(msg1)
	if err1 != nil {
		return
	}
	msg2 := []byte("world")
	_, err2 := connection.Write(msg2)
	if err2 != nil {
		return
	}
	msg3 := []byte("world")
	_, err3 := connection.Write(msg3)
	if err3 != nil {
		return
	}
	buffer := make([]byte, 4096)

	for {
		numOfBytes, err := connection.Read(buffer)
		if err != nil {
			break
		}
		msg := string(buffer[:numOfBytes])
		fmt.Println(msg)
	}
}
