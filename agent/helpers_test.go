package agent

import (
	"net/http"
	"net/http/httptest"
)

func httpPage(html string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(html)) }))
}
