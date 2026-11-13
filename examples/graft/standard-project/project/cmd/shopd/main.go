// Command shopd serves the price of an order over HTTP. It is the service the
// integration suite starts as a real process and talks to over a real socket.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"

	"example.com/shop/internal/price"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "address to listen on")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/total", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		qty, err := strconv.Atoi(q.Get("qty"))
		if err != nil {
			http.Error(w, "qty must be an integer", http.StatusBadRequest)
			return
		}
		cents, err := strconv.Atoi(q.Get("cents"))
		if err != nil {
			http.Error(w, "cents must be an integer", http.StatusBadRequest)
			return
		}
		sum, err := price.Total([]price.Line{{SKU: q.Get("sku"), Qty: qty, Cents: cents}})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int{"cents": sum})
	})

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("shopd: %v", err)
	}
	// The bound address, so that a test can ask for :0 and read the port.
	fmt.Println(ln.Addr().String())
	log.Fatal(http.Serve(ln, mux))
}
