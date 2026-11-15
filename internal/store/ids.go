package store

import "github.com/google/uuid"

// NewID is a new row's id: a UUIDv7, which starts with the time it was made.
// New rows then land together at the end of the primary-key index instead of
// all over it, and ids sort by age. Ids made before this stay as they are.
func NewID() uuid.UUID {
	id, err := uuid.NewV7()
	if err != nil { // the random source failed
		return uuid.New()
	}
	return id
}
