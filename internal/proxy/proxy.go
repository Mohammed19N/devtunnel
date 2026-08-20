package proxy

import (
	"io"
	"net/http"
)

type Proxy struct {
	targetHost string
}

func New(Host string) Proxy {
	newProxy := Proxy{
		targetHost: Host,
	}
	return newProxy
}

func (hostProxy *Proxy) BuildTargetURL(RequestURI string) (target string) {
	return hostProxy.targetHost + RequestURI
}

func CreateRequest(method string, url string, body io.Reader) (*http.Request, error) {
	newRequest, err := http.NewRequest(method, url, body)
	return newRequest, err
}

func CopyRequestHeaders(source http.Header, destination http.Header) {
	for key, values := range source {
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}

func SendRequest(request *http.Request) (*http.Response, error) {
	Client := &http.Client{}
	response, err := Client.Do(request)
	return response, err
}

func CopyResponseInfo(RW http.ResponseWriter, responseBody *http.Response) {
	CopyRequestHeaders(responseBody.Header, RW.Header())
	RW.WriteHeader(responseBody.StatusCode)
	io.Copy(RW, responseBody.Body)
}
