package mocks

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"

	"github.com/conductorone/baton-sdk/pkg/uhttp"
	"github.com/shurcooL/githubv4"
)

func MockGraphQL() *githubv4.Client {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(writer http.ResponseWriter, request *http.Request) {
				b, _ := io.ReadAll(request.Body)

				writer.Header().Set(uhttp.ContentType, "application/json")
				writer.WriteHeader(http.StatusOK)

				var filename string
				if strings.Contains(string(b), "samlIdentityProvider{id}") {
					filename = "../../test/mocks/fixtures/organization0.json"
				} else {
					filename = "../../test/mocks/fixtures/organization1.json"
				}
				data, _ := os.ReadFile(filename)
				_, err := writer.Write(data)
				if err != nil {
					return
				}
			},
		),
	)

	return githubv4.NewEnterpriseClient(server.URL, server.Client())
}
