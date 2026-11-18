// credential-key provisions the private key volume before the console starts.
package main

import (
	"flag"
	"fmt"
	"os"

	"tosub2-go-port/internal/credentials"
)

func main() {
	keyFile := flag.String("key-file", "", "private 32-byte credential key file")
	credentialRoot := flag.String("credential-root", "", "credential directory bound to this key")
	flag.Parse()
	if *keyFile == "" || *credentialRoot == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: tosub2-key-init --key-file PATH --credential-root PATH")
		os.Exit(2)
	}
	if err := credentials.InitializeFileKey(*keyFile, *credentialRoot); err != nil {
		fmt.Fprintln(os.Stderr, "credential key initialization failed:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "Private credential key is ready.")
}
