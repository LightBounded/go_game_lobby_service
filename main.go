// TODO: Update queue to be based on Stats struct or something like that
// so we can update that info
// TODO: Implement functions for income and skip cost calculation
// TODO: Write logic to update income and send to client
package main

import (
	"context"
	"log"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

var (
	players      = make([]string, 0)
	clients      = make([]*websocket.Conn, 0)
	connToPlayer = make(map[*websocket.Conn]string)
	mu           sync.Mutex
)

// WSGameMessage represents the message format from the client
type WSGameMessage struct {
	Event   string `json:"event"`
	Player  string `json:"player,omitempty"`
	Skipper string `json:"skipper,omitempty"`
	Skipped string `json:"skipped,omitempty"`
}

type User struct {
	gorm.Model
	ClerkUserId  string
	NumberOfWins uint `gorm:"default:0;uniqueIndex"`
}

type Stats struct {
	Income   uint
	SkipCost uint
	Money    uint
	Position uint
}

type Player struct {
	Stats Stats
	User  User
}

func main() {
	db, err := gorm.Open(sqlite.Open("test.db"), &gorm.Config{})
	if err != nil {
		panic("failed to connect database")
	}

	// Migrate the schema
	db.AutoMigrate(&User{})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns: []string{"*"},
		})
		if err != nil {
			log.Println("Error accepting connection:", err)
			return
		}

		mu.Lock()
		clients = append(clients, conn)
		mu.Unlock()

		// Ensure removal of connection and associated player on disconnect.
		defer func() {
			mu.Lock()
			// Remove from clients slice.
			removeClient(conn)
			// If a player was associated with this connection, remove them from players.
			player, exists := connToPlayer[conn]
			if exists {
				removePlayer(player)
				// Optionally, broadcast the playerLeave event to all other clients.
				broadcastLeave(player)
				delete(connToPlayer, conn)
			}
			mu.Unlock()
			conn.CloseNow()
		}()

		// Using a context with timeout for message reading; adjust as needed.
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()

		// Read messages in a loop.
		for {
			var msg WSGameMessage
			err = wsjson.Read(ctx, conn, &msg)
			if err != nil {
				log.Println("Error reading message:", err)
				break
			}
			handleMessage(ctx, conn, msg, db)
		}
	})

	log.Println("Server listening on :8080")
	http.ListenAndServe(":8080", nil)
}

func handleMessage(ctx context.Context, conn *websocket.Conn, msg WSGameMessage, db *gorm.DB) {
	mu.Lock()
	defer mu.Unlock()

	switch msg.Event {
	case "playerJoin":
		log.Printf("Player joined: %s", msg.Player)

		// Add the new player to the database if they don't already exist.
		db.Create(&User{ClerkUserId: msg.Player})

		// Add the new player to the list if not already present.
		if !slices.Contains(players, msg.Player) {
			players = append(players, msg.Player)
		}
		// Associate the connection with this player.
		connToPlayer[conn] = msg.Player

		// Send the current lobby state to the joining player.
		response := struct {
			Event   string   `json:"event"`
			Players []string `json:"players"`
		}{
			Event:   "lobbyState",
			Players: players,
		}
		if err := wsjson.Write(ctx, conn, response); err != nil {
			log.Println("Error sending lobby state:", err)
		}

		// Broadcast the join event to all other connected clients.
		broadcast := struct {
			Event  string `json:"event"`
			Player string `json:"player"`
		}{
			Event:  "playerJoin",
			Player: msg.Player,
		}
		for _, c := range clients {
			if c != conn {
				if err := wsjson.Write(ctx, c, broadcast); err != nil {
					log.Println("Error broadcasting player join:", err)
				}
			}
		}
	case "playerLeave":
		log.Printf("Player left: %s", msg.Player)
		// Remove the player from the players list.
		removePlayer(msg.Player)
		// Optionally, broadcast the leave event.
		broadcastLeave(msg.Player)
	case "playerSkip":
		// TODO: make sure player has enough money to cover skip cost before declaring successful skip
		// TODO: send over updated Stats stuct
		log.Printf("Player skipped: %s", msg.Skipper)
		// Broadcast the skip event to all other connected clients.
		broadcast := struct {
			Event   string `json:"event"`
			Skipper string `json:"skipper"`
			Skipped string `json:"skipped"`
		}{
			Event:   "playerSkip",
			Skipper: msg.Skipper,
			Skipped: msg.Skipped,
		}
		for _, c := range clients {
			if err := wsjson.Write(ctx, c, broadcast); err != nil {
				log.Println("Error broadcasting player skip:", err)
			}
		}
	default:
		log.Printf("Unknown event: %s", msg.Event)
	}
}

func removeClient(conn *websocket.Conn) {
	for i, c := range clients {
		if c == conn {
			clients = slices.Delete(clients, i, i+1)
			break
		}
	}
}

func removePlayer(player string) {
	for i, p := range players {
		if p == player {
			players = slices.Delete(players, i, i+1)
			break
		}
	}
}

func broadcastLeave(player string) {
	// Create a message to broadcast the leave event.
	msg := struct {
		Event  string `json:"event"`
		Player string `json:"player"`
	}{
		Event:  "playerLeave",
		Player: player,
	}
	// Use a background context for broadcasting.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	for _, c := range clients {
		if err := wsjson.Write(ctx, c, msg); err != nil {
			log.Println("Error broadcasting player leave:", err)
		}
	}
}
