package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/jinzhu/gorm"

	"github.com/dgrijalva/jwt-go"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// UnRustleLogs ...
type UnRustleLogs struct {
	config *Config
	db     *gorm.DB

	dggStates     map[string]*state
	dggStateMutex sync.RWMutex

	twitchStates     map[string]struct{}
	twitchStateMutex sync.RWMutex
}

type state struct {
	service  string
	verifier string
	time     time.Time
}

const (
	// TITLE ...
	TITLE = "UnRustleLogs"
	// TWITCHSERVICE ...
	TWITCHSERVICE = "twtch"
	// DESTINYGGSERVICE ...
	DESTINYGGSERVICE = "destinygg"
)

// jwtCustomClaims are custom claims extending default ones.
type jwtClaims struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Service     string `json:"service"`
	jwt.StandardClaims
}

func main() {
	rustle := NewUnRustleLogs()
	rustle.LoadConfig("config.toml")

	rustle.NewDatabase()

	router := gin.Default()
	router.LoadHTMLGlob("templates/*")

	router.GET("/", rustle.indexHandler)

	twitch := router.Group("/twitch")
	{
		twitch.GET("/login", rustle.TwitchLoginHandle)
		twitch.GET("/logout", rustle.TwitchLogoutHandle)
		twitch.GET("/callback", rustle.TwitchCallbackHandle)
		twitch.GET("/delete", rustle.jwtMiddleware(rustle.deleteHandler(TWITCHSERVICE), rustle.config.Twitch.Cookie))
		twitch.GET("/undelete", rustle.jwtMiddleware(rustle.undeleteHandler(TWITCHSERVICE), rustle.config.Twitch.Cookie))
	}

	dgg := router.Group("/dgg")
	{
		dgg.GET("/login", rustle.DestinyggLoginHandle)
		dgg.GET("/logout", rustle.DestinyggLogoutHandle)
		dgg.GET("/callback", rustle.DestinyggCallbackHandle)
		dgg.GET("/delete", rustle.jwtMiddleware(rustle.deleteHandler(DESTINYGGSERVICE), rustle.config.Destinygg.Cookie))
		dgg.GET("/undelete", rustle.jwtMiddleware(rustle.undeleteHandler(DESTINYGGSERVICE), rustle.config.Destinygg.Cookie))
	}

	router.Static("/assets", "./assets")

	srv := &http.Server{
		Handler: router,
		Addr:    rustle.config.Server.Address,
		// Good practice: enforce timeouts for servers you create!
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	logrus.Infof("starting server :%s", rustle.config.Server.Address)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logrus.Error(err)
		}
	}()

	c := make(chan os.Signal, 1)
	// We'll accept graceful shutdowns when quit via SIGINT (Ctrl+C)
	// SIGKILL, SIGQUIT or SIGTERM (Ctrl+/) will not be caught.
	signal.Notify(c, os.Interrupt)

	// Block until we receive our signal.
	<-c

	// Create a deadline to wait for.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*15)
	defer cancel()
	// Doesn't block if no connections, but will otherwise wait
	// until the timeout deadline.
	if err := srv.Shutdown(ctx); err != nil {
		logrus.Fatal("Server Shutdown:", err)
	}
	logrus.Info("Server exiting")

}

// NewUnRustleLogs ...
func NewUnRustleLogs() *UnRustleLogs {
	return &UnRustleLogs{
		dggStates:    make(map[string]*state),
		twitchStates: make(map[string]struct{}),
	}
}

// Payload ...
type Payload struct {
	Title  string
	Twitch struct {
		Name       string
		Email      string
		LoggedIn   bool
		IsDeleting bool
	}
	Destinygg struct {
		Name       string
		LoggedIn   bool
		IsDeleting bool
	}
	DeleteStatus string
}

func (ur *UnRustleLogs) indexHandler(c *gin.Context) {
	payload := Payload{
		Title: TITLE,
	}
	twitch, ok := ur.getUser(c, ur.config.Twitch.Cookie)
	if ok {
		payload.Twitch.Name = twitch.DisplayName
		payload.Twitch.Email = twitch.Email
		payload.Twitch.LoggedIn = true
		payload.Twitch.IsDeleting = ur.UserInDatabase(twitch.Name, TWITCHSERVICE)
	}
	dgg, ok := ur.getUser(c, ur.config.Destinygg.Cookie)
	if ok {
		payload.Destinygg.Name = dgg.DisplayName
		payload.Destinygg.LoggedIn = true
		payload.Destinygg.IsDeleting = ur.UserInDatabase(dgg.Name, DESTINYGGSERVICE)
	}
	if s := c.Query("delete"); s != "" {
		payload.DeleteStatus = s
	}
	c.HTML(http.StatusOK, "index.tmpl", payload)
}

func (ur *UnRustleLogs) deleteHandler(service string) func(*gin.Context) {
	return func(c *gin.Context) {
		user, ok := c.Get("user")
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{
				"message": "Unauthorized",
			})
			return
		}
		logrus.Infof("%s requested log deletion", user.(*jwtClaims).DisplayName)
		ur.AddUser(user.(*jwtClaims).Name, service)
		c.Redirect(http.StatusFound, "/?delete=true")
	}
}

func (ur *UnRustleLogs) undeleteHandler(service string) func(*gin.Context) {
	return func(c *gin.Context) {
		user, ok := c.Get("user")
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{
				"message": "Unauthorized",
			})
			return
		}
		logrus.Infof("%s requested to stop log deletion", user.(*jwtClaims).DisplayName)
		ur.DeleteUser(user.(*jwtClaims).Name, service)
		c.Redirect(http.StatusFound, "/?delete=false")
	}
}

func (ur *UnRustleLogs) getUser(c *gin.Context, cookie string) (*jwtClaims, bool) {
	cookie, err := c.Cookie(cookie)
	if err != nil {
		return nil, false
	}
	token, err := jwt.ParseWithClaims(cookie, &jwtClaims{}, func(token *jwt.Token) (interface{}, error) {
		return []byte(ur.config.Server.JWTSecret), nil
	})
	if err != nil {
		logrus.Error(err)
		ur.deleteCookie(c, cookie)
		return nil, false
	}

	if claims, ok := token.Claims.(*jwtClaims); ok && token.Valid {
		return claims, true
	}
	return nil, false
}

func (ur *UnRustleLogs) jwtMiddleware(fn func(*gin.Context), cookie string) func(*gin.Context) {
	return func(c *gin.Context) {
		cookie, err := c.Cookie(cookie)
		if err != nil {
			logrus.Error(err)
			c.JSON(http.StatusUnauthorized, gin.H{
				"message": "Unauthorized",
			})
			return
		}
		token, err := jwt.ParseWithClaims(cookie, &jwtClaims{}, func(token *jwt.Token) (interface{}, error) {
			return []byte(ur.config.Server.JWTSecret), nil
		})
		if err != nil {
			logrus.Error(err)
			ur.deleteCookie(c, cookie)
			c.JSON(http.StatusUnauthorized, gin.H{
				"message": "error",
			})
			return
		}

		if claims, ok := token.Claims.(*jwtClaims); ok && token.Valid {
			c.Set("user", claims)
			fn(c)
		} else {
			switch cookie {
			case ur.config.Twitch.Cookie:
				c.Redirect(http.StatusTemporaryRedirect, "/twitch/logout")
			case ur.config.Destinygg.Cookie:
				c.Redirect(http.StatusTemporaryRedirect, "/dgg/logout")
			default:
				c.Redirect(http.StatusTemporaryRedirect, "/")
			}
		}
	}
}

func (ur *UnRustleLogs) addDggState(s, verifier string) {
	ur.dggStateMutex.Lock()
	defer ur.dggStateMutex.Unlock()
	ur.dggStates[s] = &state{
		verifier: verifier,
		service:  TWITCHSERVICE,
		time:     time.Now().UTC(),
	}
	go func() {
		time.Sleep(time.Minute * 5)
		ur.deleteDggState(s)
	}()
}

func (ur *UnRustleLogs) hasDggState(state string) (string, bool) {
	ur.dggStateMutex.RLock()
	defer ur.dggStateMutex.RUnlock()
	s, ok := ur.dggStates[state]
	return s.verifier, ok
}

func (ur *UnRustleLogs) deleteDggState(state string) {
	ur.dggStateMutex.Lock()
	defer ur.dggStateMutex.Unlock()
	_, ok := ur.dggStates[state]
	if ok {
		logrus.Infof("deleting dgg state %s", state)
		delete(ur.dggStates, state)
	}
}

func (ur *UnRustleLogs) addTwitchState(s string) {
	ur.twitchStateMutex.Lock()
	defer ur.twitchStateMutex.Unlock()
	ur.twitchStates[s] = struct{}{}
	go func() {
		time.Sleep(time.Minute * 5)
		ur.deleteTwitchState(s)
	}()
}

func (ur *UnRustleLogs) hasTwitchState(state string) bool {
	ur.twitchStateMutex.RLock()
	defer ur.twitchStateMutex.RUnlock()
	_, ok := ur.twitchStates[state]
	return ok
}

func (ur *UnRustleLogs) deleteTwitchState(state string) {
	ur.twitchStateMutex.Lock()
	defer ur.twitchStateMutex.Unlock()
	_, ok := ur.twitchStates[state]
	if ok {
		logrus.Infof("deleting twitch state %s", state)
		delete(ur.twitchStates, state)
	}
}