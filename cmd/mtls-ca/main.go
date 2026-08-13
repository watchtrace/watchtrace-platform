package main

import (
	"flag"
	"fmt"
	"github.com/watchtrace/watchtrace-platform/internal/mtlspki"
	"os"
	"strings"
	"time"
)

func main() {
	mode := flag.String("mode", "issue", "init or issue")
	cert := flag.String("ca-cert", "worker-root.crt", "offline root certificate")
	key := flag.String("ca-key", "worker-root.key", "offline root private key")
	pool := flag.String("pool", "", "worker pool ID")
	out := flag.String("out-prefix", "worker", "issued identity prefix")
	flag.Parse()
	var err error
	if *mode == "init" {
		var c, k []byte
		c, k, err = mtlspki.NewRoot(time.Now())
		if err == nil {
			err = os.WriteFile(*cert, c, 0644)
		}
		if err == nil {
			err = os.WriteFile(*key, k, 0600)
		}
	} else {
		var c, k, issued, private []byte
		var serial string
		c, err = os.ReadFile(*cert)
		if err == nil {
			k, err = os.ReadFile(*key)
		}
		if err == nil {
			issued, private, serial, err = mtlspki.Issue(c, k, strings.TrimSpace(*pool), time.Now(), 30*24*time.Hour)
		}
		if err == nil {
			err = os.WriteFile(*out+".crt", issued, 0644)
		}
		if err == nil {
			err = os.WriteFile(*out+".key", private, 0600)
		}
		if err == nil {
			fmt.Println(serial)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mTLS CA operation failed")
		os.Exit(1)
	}
}
