package main

import (
	"fmt"
	"log"
	"net/http"
	"os"

	"devtunnel/internal/proxy"
	"devtunnel/internal/tunnel"
	"devtunnel/localServer"
)

func main() {
	if len(os.Args) <= 1 {
		fmt.Println("Please provide the target Host while ruuning the program Like :")
		fmt.Println("go run . localhost:3000")
		os.Exit(1)
	}
	targetHost := "http://" + os.Args[1]
	hostProxy := proxy.New(targetHost)
	go localServer.Locally()
	go tunnel.StartServer(":9000")
	go tunnel.StartClient("localhost:9000")
	handler := func(RW http.ResponseWriter, req *http.Request) {
		target := hostProxy.BuildTargetURL(req.RequestURI)
		newReq, err := proxy.CreateRequest(req.Method, target, req.Body)
		if err != nil {
			fmt.Println(err)
			return
		}
		proxy.CopyRequestHeaders(req.Header, newReq.Header)
		resp, err := proxy.SendRequest(newReq)
		if err != nil {
			fmt.Println(err)
		} else {
			defer resp.Body.Close()
			proxy.CopyResponseInfo(RW, resp)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", handler)
	log.Fatal(http.ListenAndServe(":8080", mux))
}
