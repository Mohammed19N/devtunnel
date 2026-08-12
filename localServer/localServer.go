package localServer

import (
	"fmt"
	"io"
	"log"
	"net/http"
)

func Locally() {
	handler := func(RW http.ResponseWriter, req *http.Request) {
		fmt.Println("Local server received:", req.RequestURI)
		io.WriteString(RW, "Hello from the local application\n")
		fmt.Println(req.Method, req.RequestURI)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/test", handler)
	log.Fatal(http.ListenAndServe(":3000", mux))
}
