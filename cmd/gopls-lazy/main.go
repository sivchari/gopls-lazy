// Command gopls-lazy is an LSP stdio proxy that sits between an editor and
// gopls and dynamically narrows the gopls workspace to the directories the
// user is actually editing. See the goplslazy package for details.
package main

import (
	"os"

	goplslazy "github.com/sivchari/gopls-lazy"
)

func main() {
	os.Exit(goplslazy.Run())
}
