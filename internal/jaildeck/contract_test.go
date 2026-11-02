//go:build contract

package jaildeck_test

import (
	"os"
	"strings"
	"testing"

	"github.com/ottermq/otter_cloud/internal/jaildeck"
)

// TestContract runs the contract against the real JailDeck on a node, before
// Otter is moved to a new JailDeck version (JD-016's change policy):
//
//	GOOS=freebsd go test -c -tags contract -o jaildeck-contract ./internal/jaildeck
//	OTTER_JAILDECK_SOCKET=/var/run/jaildeck.sock \
//	OTTER_JAILDECK_TOKEN_FILE=<the worker's token> \
//	OTTER_JAILDECK_CONTRACT_JAIL=<a running cluster jail> \
//	OTTER_JAILDECK_CONTRACT_DENIED_JAIL=<a running jail of another role> \
//	  ./jaildeck-contract -test.v
//
// It only lists, reads and runs echo, printenv, sh -c "exit 3" and sleep in
// the cluster jail, and tries true in the other, which the worker's limited
// token must be refused (#169); it creates and destroys nothing.
func TestContract(t *testing.T) {
	socket, tokenFile := os.Getenv("OTTER_JAILDECK_SOCKET"), os.Getenv("OTTER_JAILDECK_TOKEN_FILE")
	jail, denied := os.Getenv("OTTER_JAILDECK_CONTRACT_JAIL"), os.Getenv("OTTER_JAILDECK_CONTRACT_DENIED_JAIL")
	if socket == "" || tokenFile == "" || jail == "" || denied == "" {
		t.Fatal("set OTTER_JAILDECK_SOCKET, OTTER_JAILDECK_TOKEN_FILE, OTTER_JAILDECK_CONTRACT_JAIL and OTTER_JAILDECK_CONTRACT_DENIED_JAIL")
	}
	token, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	contract(t, jaildeck.New(socket, strings.TrimSpace(string(token))), jaildeck.New(socket, "jdt_not_a_token"), jail, denied)
}
