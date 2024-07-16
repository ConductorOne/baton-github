package mocks

import (
	"fmt"
	"io"
	"log"
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
				writer.WriteHeader(200)

				dir, err := os.Getwd()
				if err != nil {
					log.Fatal(err)
				}
				fmt.Println(dir)

				var filename string
				if strings.Contains(string(b), "samlIdentityProvider{id}") {
					filename = "../../test/mocks/fixtures/organization0.json"
				} else {
					filename = "../../test/mocks/fixtures/organization1.json"
				}
				data, _ := os.ReadFile(filename)
				writer.Write(data)
			},
		),
	)

	return githubv4.NewEnterpriseClient(server.URL, server.Client())
}
