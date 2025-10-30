package main

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/nobidev/go-git/v6"
	. "github.com/nobidev/go-git/v6/_examples"
	"github.com/nobidev/go-git/v6/plumbing/transport"
	githttp "github.com/nobidev/go-git/v6/plumbing/transport/http"
	"github.com/nobidev/go-git/v6/storage/memory"
)

// Here is an example to configure http client according to our own needs.
func main() {
	CheckArgs("<url>")
	url := os.Args[1]

	// Create a custom http(s) client with your config
	customClient := &http.Client{
		// accept any certificate (might be useful for testing)
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},

		// 15 second timeout
		Timeout: 15 * time.Second,

		// don't follow redirect
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// Override http(s) default protocol to use our custom client
	transport.Register("https",
		githttp.NewTransport(&githttp.TransportOptions{Client: customClient}))

	// Clone repository using the new client if the protocol is https://
	Info("git clone %s", url)

	r, err := git.Clone(memory.NewStorage(), nil, &git.CloneOptions{URL: url})
	CheckIfError(err)

	// Retrieve the branch pointed by HEAD
	Info("git rev-parse HEAD")

	head, err := r.Head()
	CheckIfError(err)
	fmt.Println(head.Hash())
}
