package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	"devtunnel/localServer"
)

func main() {
	if len(os.Args) <= 1 {
		fmt.Println("Please provide the target Host while ruuning the program Like :")
		fmt.Println("go run . localhost:3000")
		os.Exit(1)
	}
	targetHost := "http://" + os.Args[1]
	go localServer.Locally()
	handler := func(RW http.ResponseWriter, req *http.Request) {
		//resp, err := http.Get("http://localhost:3000" + req.RequestURI)
		target := targetHost + req.RequestURI
		fmt.Println("Forwarding to:", target)

		resp, err := http.Get(target)
		if err != nil {
			fmt.Println(err)
		} else {
			defer resp.Body.Close()
			io.Copy(RW, resp.Body)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", handler)
	log.Fatal(http.ListenAndServe(":8080", mux))
}
