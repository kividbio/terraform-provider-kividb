package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/kividb/terraform-provider-kividb/internal/provider"
)

// Set by the release build; "dev" when run from source.
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run with support for debuggers like delve")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: "registry.terraform.io/kividb/kividb",
		Debug:   debug,
	})
	if err != nil {
		log.Fatal(err)
	}
}
