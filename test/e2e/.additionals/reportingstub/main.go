// reportingstub is the e2e test reporting service for the billing-api compose
// stand: a gRPC ReportingService with deterministic fixtures (not included in
// the production image).
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/cbl/cbl-billing-api/test/reportingstub"
)

// main starts the test ReportingService until a signal is received.
func main() {
	addr := os.Getenv("STUB_GRPC_ADDR")
	if addr == "" {
		addr = ":9000"
	}
	server := reportingstub.New()
	actual, err := server.Start(addr)
	if err != nil {
		log.Fatal(err)
	}
	defer server.Stop()
	fmt.Println("reporting stub grpc started on", actual)

	wait := make(chan os.Signal, 1)
	signal.Notify(wait, os.Interrupt, syscall.SIGTERM)
	<-wait
}
