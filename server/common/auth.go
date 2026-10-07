package common

import (
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/go-cache"
	"github.com/golang-jwt/jwt/v4"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm/clause"
)

var SecretKey []byte

type UserClaims struct {
	Username string `json:"username"`
	PwdTS    int64  `json:"pwd_ts"`
	jwt.RegisteredClaims
}

var validTokenCache = cache.NewMemCache[bool]()

func GenerateToken(user *model.User) (tokenString string, err error) {
	claim := UserClaims{
		Username: user.Username,
		PwdTS:    user.PwdTS,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Duration(conf.Conf.TokenExpiresIn) * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			NotBefore: jwt.NewNumericDate(time.Now()),
		}}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claim)
	tokenString, err = token.SignedString(SecretKey)
	if err != nil {
		return "", err
	}
	validTokenCache.Set(tokenString, true)
	persistValidToken(tokenString)
	return tokenString, err
}

// persistValidToken stores the token so the cache can be rebuilt after a
// restart. DB failures only downgrade to the old restart-invalidates behavior.
func persistValidToken(tokenString string) {
	if err := db.GetDb().Clauses(clause.OnConflict{DoNothing: true}).
		Create(&model.ValidToken{Token: tokenString}).Error; err != nil {
		log.Warnf("failed to persist auth token: %v", err)
	}
}

// LoadValidTokens restores the in-memory token cache from the database and
// prunes rows that are expired or no longer verifiable. Must be called after
// SecretKey is initialized.
func LoadValidTokens() {
	var rows []model.ValidToken
	if err := db.GetDb().Find(&rows).Error; err != nil {
		log.Warnf("failed to load valid tokens: %v", err)
		return
	}
	var stale []string
	live := 0
	for _, r := range rows {
		if _, err := parseClaims(r.Token); err != nil {
			stale = append(stale, r.Token)
			continue
		}
		validTokenCache.Set(r.Token, true)
		live++
	}
	if len(stale) > 0 {
		if err := db.GetDb().Where("token IN ?", stale).Delete(&model.ValidToken{}).Error; err != nil {
			log.Warnf("failed to prune %d stale tokens: %v", len(stale), err)
		}
	}
	log.Infof("restored %d valid auth tokens, pruned %d stale", live, len(stale))
}

func parseClaims(tokenString string) (*UserClaims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &UserClaims{}, func(token *jwt.Token) (interface{}, error) {
		return SecretKey, nil
	})
	if err != nil {
		return nil, err
	}
	if claims, ok := token.Claims.(*UserClaims); ok && token.Valid {
		return claims, nil
	}
	return nil, errors.New("couldn't handle this token")
}

func ParseToken(tokenString string) (*UserClaims, error) {
	if IsTokenInvalidated(tokenString) {
		return nil, errors.New("token is invalidated")
	}
	claims, err := parseClaims(tokenString)
	if err != nil {
		if ve, ok := err.(*jwt.ValidationError); ok {
			if ve.Errors&jwt.ValidationErrorMalformed != 0 {
				return nil, errors.New("that's not even a token")
			} else if ve.Errors&jwt.ValidationErrorExpired != 0 {
				return nil, errors.New("token is expired")
			} else if ve.Errors&jwt.ValidationErrorNotValidYet != 0 {
				return nil, errors.New("token not active yet")
			}
		}
		return nil, errors.New("couldn't handle this token")
	}
	return claims, nil
}

func InvalidateToken(tokenString string) error {
	if tokenString == "" {
		return nil // don't invalidate empty guest token
	}
	validTokenCache.Del(tokenString)
	if err := db.GetDb().Where("token = ?", tokenString).
		Delete(&model.ValidToken{}).Error; err != nil {
		log.Warnf("failed to delete persisted token: %v", err)
	}
	return nil
}

func IsTokenInvalidated(tokenString string) bool {
	_, ok := validTokenCache.Get(tokenString)
	return !ok
}
