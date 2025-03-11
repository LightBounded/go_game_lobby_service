package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r,
			// TODO: Add stricter origin checks
			&websocket.AcceptOptions{
				OriginPatterns: []string{"*"},
			})
		if err != nil {
			log.Println(err)
			return
		}
		defer conn.CloseNow()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
		defer cancel()

		var v interface{}
		err = wsjson.Read(ctx, conn, &v)
		if err != nil {
			log.Println(err)
			return
		}

		log.Printf("Received: %v", v)

		conn.Close(websocket.StatusNormalClosure, "")
	})

	http.ListenAndServe(":8080", nil)
}
