// reportingstub - e2e-сервис тестовой отчетности для compose-стенда
// billing-api: gRPC ReportingService с детерминированными фикстурами
// (в продуктовый образ не входит).
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/cbl/cbl-billing-api/test/reportingstub"
)

// main поднимает тестовый ReportingService до сигнала.
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
