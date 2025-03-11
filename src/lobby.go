package main

type Player struct {
	UserId string
}

type Lobby struct {
	Players []Player
}

func (l *Lobby) AddPlayer(p Player) {
	l.Players = append(l.Players, p)
}

func (l *Lobby) RemovePlayer(p Player) {
	for i, player := range l.Players {
		if player.UserId == p.UserId {
			l.Players = append(l.Players[:i], l.Players[i+1:]...)
			return
		}
	}
}

func (l *Lobby) GetPlayers() []Player {
	return l.Players
}
