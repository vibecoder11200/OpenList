package model

// ValidToken persists the set of issued auth tokens so that the in-memory
// token cache can be restored after a restart; without it every restart
// invalidates all sessions ("token is invalidated").
type ValidToken struct {
	Token string `json:"token" gorm:"primaryKey;type:text"`
}
