package server

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

func msTime(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func randomKey() string {
	b := make([]byte, randomKeyBytes)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
