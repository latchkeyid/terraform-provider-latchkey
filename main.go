package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/latchkeyid/terraform-provider-latchkey/internal/provider"
)

// version is set by the release build (goreleaser -X main.version=...).
var version = "dev"

func main() {
	debug := flag.Bool("debug", false, "run with support for debuggers like delve")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: "registry.terraform.io/latchkeyid/latchkey",
		Debug:   *debug,
	})
	if err != nil {
		log.Fatal(err)
	}
}
