package localServer

import (
	"fmt"
	"io"
	"log"
	"net/http"
)

func Locally() {
	handler := func(RW http.ResponseWriter, req *http.Request) {
		io.WriteString(RW, "Hello from the local application\n the body is : ")
		reqBody, err := io.ReadAll(req.Body)
		if err != nil {
			fmt.Println(err)
		}
		fmt.Println(req.Method, req.RequestURI, string(reqBody), req.Header)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/test", handler)
	log.Fatal(http.ListenAndServe(":3000", mux))
}
