package sfu

// Participant is a user in a room (02 §5.1): one or more Conns (a desktop app's publisher and its viewer are one
// person) and the shares they publish, at most four (02 §12). It ends with its last Conn. Its maps are guarded by
// Room.mu.
type Participant struct {
	id     ParticipantID
	user   UserID
	room   *Room
	conns  map[ConnID]*Conn
	shares map[ShareID]*Share
}

func newParticipant(room *Room, id ParticipantID, user UserID) *Participant {
	return &Participant{id: id, user: user, room: room, conns: map[ConnID]*Conn{}, shares: map[ShareID]*Share{}}
}
