// TODO: Update queue to be based on Stats struct or something like that
// so we can update that info
// TODO: Implement functions for income and skip cost calculation
// TODO: Write logic to update income and send to client
// TODO: Update clients with new info when someone is skipped
// idea for income: 100 * (1 - (n/500))^5
// idea for skip cost: 250+(10 * (1 + .012)^(256-n))
package main

import (
	"context"
	"log"
	"math"
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
	playerStats  = make(map[string]Stats) // Map to store player stats
	mu           sync.Mutex
)

// WSGameMessage represents the message format from the client
type WSGameMessage struct {
	Event       string             `json:"event"`
	Player      string             `json:"player,omitempty"`
	PlayerName  string             `json:"playerName,omitempty"` // Added field for username
	Skipper     string             `json:"skipper,omitempty"`
	Skipped     string             `json:"skipped,omitempty"`
	Leaderboard []LeaderboardEntry `json:"leaderboard,omitempty"`
}

type User struct {
	gorm.Model
	ClerkUserId  string `gorm:"uniqueIndex"` // Make ClerkUserId unique instead
	Username     string
	NumberOfWins uint `gorm:"default:0"` // Remove uniqueIndex from here
}

type Stats struct {
	Income   uint
	SkipCost uint
	Money    uint
	Position uint
	Cycles   uint // Number of times a player has cycled through the queue
}

// LeaderboardEntry represents a single entry in the leaderboard
type LeaderboardEntry struct {
	PlayerId   string `json:"playerId"`
	PlayerName string `json:"playerName"` // Added field for username in leaderboard
	Cycles     uint   `json:"cycles"`
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

	// Start a ticker to update income periodically
	ticker := time.NewTicker(10 * time.Second)
	go func() {
		for range ticker.C {
			updatePlayerIncomes()
		}
	}()

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
		log.Printf("Player joined: %s with name: %s", msg.Player, msg.PlayerName)

		// Check if the user already exists in the database
		var existingUser User
		result := db.Where("clerk_user_id = ?", msg.Player).First(&existingUser)

		if result.Error != nil {
			// User doesn't exist, create a new one
			newUser := User{ClerkUserId: msg.Player, Username: msg.PlayerName}
			db.Create(&newUser)
		} else if msg.PlayerName != "" && existingUser.Username != msg.PlayerName {
			// User exists but username has changed, update it
			existingUser.Username = msg.PlayerName
			db.Save(&existingUser)
		}

		// Add the new player to the list if not already present.
		if !slices.Contains(players, msg.Player) {
			players = append(players, msg.Player)

			// Initialize player stats with position at the end of the queue
			position := uint(len(players) - 1)
			playerStats[msg.Player] = Stats{
				Income:   calculateIncome(position),
				SkipCost: calculateSkipCost(position),
				Money:    1000, // Give new players a starting amount
				Position: position,
				Cycles:   0, // Initialize cycles to 0
			}
		} else {
			// Player reconnected, ensure they have stats
			if _, exists := playerStats[msg.Player]; !exists {
				// Find their position
				position := uint(0)
				for i, p := range players {
					if p == msg.Player {
						position = uint(i)
						break
					}
				}

				playerStats[msg.Player] = Stats{
					Income:   calculateIncome(position),
					SkipCost: calculateSkipCost(position),
					Money:    1000, // Give reconnected players their money back
					Position: position,
					Cycles:   0, // Initialize cycles to 0
				}
				// Update username if provided
				if msg.PlayerName != "" {
					var user User
					db.Where("clerk_user_id = ?", msg.Player).First(&user)
					user.Username = msg.PlayerName
					db.Save(&user)
				}
			}
		}

		// Associate the connection with this player.
		connToPlayer[conn] = msg.Player

		// Send the current lobby state to the joining player.
		// Create leaderboard data to include in the response
		leaderboard := make([]LeaderboardEntry, 0, len(players))
		for _, p := range players {
			stats := playerStats[p]
			// Look up username from database
			var user User
			result := db.Where("clerk_user_id = ?", p).First(&user)

			username := p // Default to ID if username not found
			if result.Error == nil && user.Username != "" {
				username = user.Username
			}

			leaderboard = append(leaderboard, LeaderboardEntry{
				PlayerId:   p,
				PlayerName: username,
				Cycles:     stats.Cycles,
			})
		}

		// Sort the leaderboard by cycles in descending order (highest cycles first)
		slices.SortFunc(leaderboard, func(a, b LeaderboardEntry) int {
			if a.Cycles > b.Cycles {
				return -1 // Highest cycles first
			} else if a.Cycles < b.Cycles {
				return 1
			}
			return 0
		})

		response := struct {
			Event       string             `json:"event"`
			Players     []string           `json:"players"`
			Stats       Stats              `json:"stats"`
			Leaderboard []LeaderboardEntry `json:"leaderboard"`
		}{
			Event:       "lobbyState",
			Players:     players,
			Stats:       playerStats[msg.Player],
			Leaderboard: leaderboard,
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
		log.Printf("Player skip request: %s wants to skip %s", msg.Skipper, msg.Skipped)

		// Get the skipper's stats
		skipperStats, exists := playerStats[msg.Skipper]
		if !exists {
			log.Printf("Skipper %s not found in player stats", msg.Skipper)
			return
		}

		// Check if the skipper has enough money to cover the skip cost
		if skipperStats.Money < skipperStats.SkipCost {
			// Insufficient funds - notify the player
			insufficientFundsMsg := struct {
				Event   string `json:"event"`
				Message string `json:"message"`
			}{
				Event:   "skipFailed",
				Message: "Insufficient funds to skip",
			}

			if err := wsjson.Write(ctx, conn, insufficientFundsMsg); err != nil {
				log.Println("Error sending insufficient funds message:", err)
			}
			return
		}

		// Process the skip - deduct money
		skipperStats.Money -= skipperStats.SkipCost
		playerStats[msg.Skipper] = skipperStats

		// Find the positions of the skipper and skipped player
		skipperPos := -1
		skippedPos := -1

		for i, p := range players {
			if p == msg.Skipper {
				skipperPos = i
			}
			if p == msg.Skipped {
				skippedPos = i
			}
		}

		if skipperPos == -1 || skippedPos == -1 {
			log.Printf("Could not find positions for skipper or skipped")
			return
		}

		// Swap the positions
		players[skipperPos], players[skippedPos] = players[skippedPos], players[skipperPos]

		// Update positions in stats
		for i, p := range players {
			stats := playerStats[p]

			// Check if a player was in position 0 (first place) and now moved to the end
			// This is a cycle completion
			if p == msg.Skipper && skipperPos == 0 && skippedPos == len(players)-1 {
				stats.Cycles++
				log.Printf("Player %s completed a cycle! Total cycles: %d", p, stats.Cycles)

				// After a cycle completion, broadcast the updated leaderboard
				defer broadcastLeaderboard()
			}

			stats.Position = uint(i)
			stats.Income = calculateIncome(stats.Position)
			stats.SkipCost = calculateSkipCost(stats.Position)
			playerStats[p] = stats
		}

		// Broadcast the skip event to all connected clients
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

		// Broadcast updated stats to reflect the new positions
		broadcastStats()
	case "requestLeaderboard":
		log.Printf("Leaderboard requested by player: %s", connToPlayer[conn])
		// Send the current leaderboard to the requesting client
		broadcastLeaderboard()
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

// calculateIncome calculates income based on position
// Formula: 100 * (1 - (n/500))^5
func calculateIncome(position uint) uint {
	// Higher positions (closer to 0) get more income
	n := float64(position)
	income := 100 * math.Pow(1-(n/500), 5)
	return uint(math.Max(math.Round(income), 1)) // Ensure minimum income of 1
}

// calculateSkipCost calculates the cost to skip a player
// Formula: 250+(10 * (1 + .012)^(256-n))
func calculateSkipCost(position uint) uint {
	n := float64(position)
	skipCost := 250 + (10 * math.Pow(1.012, 256-n))
	return uint(math.Round(skipCost))
}

// updatePlayerIncomes updates all players' incomes and broadcasts the updates
func updatePlayerIncomes() {
	mu.Lock()
	defer mu.Unlock()

	// Skip if no players
	if len(players) == 0 {
		return
	}

	// Update all player incomes
	for i, player := range players {
		stats := playerStats[player]
		stats.Position = uint(i)
		stats.Income = calculateIncome(stats.Position)
		stats.SkipCost = calculateSkipCost(stats.Position)
		stats.Money += stats.Income // Add income
		playerStats[player] = stats
	}

	// Broadcast updated stats to all clients
	broadcastStats()
}

// broadcastStats sends updated stats to all clients
func broadcastStats() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	for _, conn := range clients {
		player := connToPlayer[conn]
		if player == "" {
			continue
		}

		stats := playerStats[player]

		statsMsg := struct {
			Event string `json:"event"`
			Stats Stats  `json:"stats"`
		}{
			Event: "statsUpdate",
			Stats: stats,
		}

		if err := wsjson.Write(ctx, conn, statsMsg); err != nil {
			log.Println("Error broadcasting stats update:", err)
		}
	}
}

// broadcastLeaderboard sends the leaderboard to all clients
func broadcastLeaderboard() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	db, err := gorm.Open(sqlite.Open("test.db"), &gorm.Config{})
	if err != nil {
		log.Println("Error opening database in broadcastLeaderboard:", err)
		return
	}

	leaderboard := make([]LeaderboardEntry, 0, len(players))
	for _, player := range players {
		stats := playerStats[player]

		// Look up username from database
		var user User
		result := db.Where("clerk_user_id = ?", player).First(&user)

		username := player // Default to ID if username not found
		if result.Error == nil && user.Username != "" {
			username = user.Username
		}

		leaderboard = append(leaderboard, LeaderboardEntry{
			PlayerId:   player,
			PlayerName: username,
			Cycles:     stats.Cycles,
		})
	}

	// Sort the leaderboard by cycles in descending order (highest cycles first)
	slices.SortFunc(leaderboard, func(a, b LeaderboardEntry) int {
		if a.Cycles > b.Cycles {
			return -1 // Highest cycles first
		} else if a.Cycles < b.Cycles {
			return 1
		}
		return 0
	})

	for _, conn := range clients {
		leaderboardMsg := struct {
			Event       string             `json:"event"`
			Leaderboard []LeaderboardEntry `json:"leaderboard"`
		}{
			Event:       "leaderboardUpdate",
			Leaderboard: leaderboard,
		}

		if err := wsjson.Write(ctx, conn, leaderboardMsg); err != nil {
			log.Println("Error broadcasting leaderboard update:", err)
		}
	}
}
