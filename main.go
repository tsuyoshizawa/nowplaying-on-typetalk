package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	typetalk "github.com/nulab/go-typetalk/v3/typetalk/v1"
	uuid "github.com/satori/go.uuid"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/typetalk-gadget/go-typetalk-token-source/source"
	"github.com/vvatanabe/spotify-playing-stream/stream"
	"github.com/zmb3/spotify"
	"golang.org/x/oauth2"
)

const (
	cmdName     = "nowplaying-on-typetalk"
	defaultPort = 18080

	flagNameDebug                = "debug"
	flagNameConfig               = "config"
	flagNameTypetalkClientID     = "typetalk_client_id"
	flagNameTypetalkClientSecret = "typetalk_client_secret"
	flagNameTypetalkSpaceKey     = "typetalk_space_key"
	flagNameSpotifyClientID      = "spotify_client_id"
	flagNameSpotifyClientSecret  = "spotify_client_secret"
	flagNameStatusEmoji          = "status_emoji"
	flagNamePort                 = "port"
)

type Config struct {
	Debug                bool   `mapstructure:"debug"`
	TypetalkClientID     string `mapstructure:"typetalk_client_id"`
	TypetalkClientSecret string `mapstructure:"typetalk_client_secret"`
	TypetalkSpaceKey     string `mapstructure:"typetalk_space_key"`
	SpotifyClientID      string `mapstructure:"spotify_client_id"`
	SpotifyClientSecret  string `mapstructure:"spotify_client_secret"`
	StatusEmoji          string `mapstructure:"status_emoji"`
	Port                 int    `mapstructure:"port"`
}

var (
	config Config
	dotDir string
)

func main() {

	home, err := os.UserHomeDir()
	if err != nil {
		printFatal("UserHomeDir:", err)
	}

	dotDir = getDotDir(home)
	err = os.MkdirAll(dotDir, 0700)
	if err != nil {
		printFatal("MkdirAll:", err)
	}
	defaultConfigFile := filepath.Join(dotDir, "config.yaml")
	if !exists(defaultConfigFile) {
		defaultConfigFile = filepath.Join(dotDir, "config.yml")
		if !exists(defaultConfigFile) {
			_, err = os.Create(defaultConfigFile)
			if err != nil {
				printFatal("Create:", err)
			}

		}
	}

	rootCmd := &cobra.Command{
		Use:     cmdName,
		Run:     run,
		Version: FmtVersion(),
	}

	flags := rootCmd.PersistentFlags()

	flags.Bool(flagNameDebug, false, "debug mode")
	flags.StringP(flagNameConfig, "c", defaultConfigFile, "config file path")
	flags.String(flagNameTypetalkClientID, "", "typetalk client id [TYPETALK_CLIENT_ID]")
	flags.String(flagNameTypetalkClientSecret, "", "typetalk client secret [TYPETALK_CLIENT_SECRET]")
	flags.String(flagNameTypetalkSpaceKey, "", "typetalk space key [TYPETALK_SPACE_KEY]")
	flags.String(flagNameSpotifyClientID, "", "spotify client id [SPOTIFY_CLIENT_ID]")
	flags.String(flagNameSpotifyClientSecret, "", "spotify client secret [SPOTIFY_CLIENT_SECRET]")
	flags.String(flagNameStatusEmoji, ":musical_note:", "typetalk status emoji [STATUS_EMOJI]")
	flags.Int(flagNamePort, defaultPort, "port number for OAuth")

	_ = viper.BindPFlag(flagNameDebug, flags.Lookup(flagNameDebug))
	_ = viper.BindPFlag(flagNameTypetalkClientID, flags.Lookup(flagNameTypetalkClientID))
	_ = viper.BindPFlag(flagNameTypetalkClientSecret, flags.Lookup(flagNameTypetalkClientSecret))
	_ = viper.BindPFlag(flagNameTypetalkSpaceKey, flags.Lookup(flagNameTypetalkSpaceKey))
	_ = viper.BindPFlag(flagNameSpotifyClientID, flags.Lookup(flagNameSpotifyClientID))
	_ = viper.BindPFlag(flagNameSpotifyClientSecret, flags.Lookup(flagNameSpotifyClientSecret))
	_ = viper.BindPFlag(flagNameStatusEmoji, flags.Lookup(flagNameStatusEmoji))
	_ = viper.BindPFlag(flagNamePort, flags.Lookup(flagNamePort))

	cobra.OnInitialize(func() {
		configFile, err := flags.GetString(flagNameConfig)
		if err != nil {
			printFatal(err)
		}
		viper.SetConfigFile(configFile)
		viper.SetConfigType("yaml")
		// viper.SetEnvPrefix(envPrefix)
		viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
		viper.AutomaticEnv()
		if err := viper.ReadInConfig(); err != nil {
			printFatal("failed to read config", err)
		}

		if err := viper.Unmarshal(&config); err != nil {
			printFatal("failed to unmarshal config", err)
		}

	})

	if err := rootCmd.Execute(); err != nil {
		printFatal(err)
	}
}

var (
	// redirectURI is the OAuth redirect URI for the application.
	// You must register an application at Spotify's developer portal
	// and enter this value.
	// http://localhost:18080/nowplaying-on-typetalk
	redirectURI string
	auth        spotify.Authenticator
	state       = uuid.NewV4().String()
	ch          = make(chan *spotify.Client)
)

func run(c *cobra.Command, args []string) {

	// printDebug(fmt.Sprintf("config: %#v\n", config))

	redirectURI = fmt.Sprintf("http://localhost:%d/%s", config.Port, cmdName)
	auth = spotify.NewAuthenticator(redirectURI, spotify.ScopeUserReadCurrentlyPlaying)
	auth.SetAuthInfo(config.SpotifyClientID, config.SpotifyClientSecret)

	var sc *spotify.Client
	defer func() {
		if sc == nil {
			return
		}
		tok, err := sc.Token()
		if err != nil {
			printError(err)
			return
		}
		err = saveSpotifyTokenToFile(dotDir, tok)
		if err != nil {
			printError(err)
			return
		}
	}()

	if tok, err := getSpotifyTokenFromFile(dotDir); err != nil {
		printInfo(err)
	} else {
		sc, err = newSpotify(&auth, tok)
		if err != nil {
			printError(err)
		}
	}

	if sc == nil {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", config.Port))
		if err != nil {
			printFatal(err)
		}

		// first start an HTTP server
		mux := http.NewServeMux()
		// pattern /nowplaying-on-typetalk
		mux.HandleFunc("/"+cmdName, completeAuth)
		srv := http.Server{
			Handler: mux,
		}
		go func() {
			err := srv.Serve(ln)
			if err != nil && err != http.ErrServerClosed {
				printError(err)
			}
		}()

		authURL := auth.AuthURL(state)
		err = openBrowser(authURL)
		if err != nil {
			printFatal(err)
		}

		printDebug("Logged in to Spotify by visiting the following page in your browser:", authURL)

		// wait for auth to complete
		sc = <-ch

		go func() {
			err := srv.Shutdown(context.Background())
			if err != nil {
				printError(err)
			}
			ln.Close()
		}()
	}

	tc := newTypetalk(config.TypetalkClientID, config.TypetalkClientSecret, "my")

	sps := stream.Stream{
		Conn: sc,
		Handler: &handler{
			tc:       tc,
			spaceKey: config.TypetalkSpaceKey,
			emoji:    config.StatusEmoji,
		},
		LoggerFunc: printError,
	}

	go func() {
		printInfo("start to subscribe spotify playing stream")
		err := sps.Subscribe()
		if err != nil {
			printError(err)
		}
	}()

	sigint := make(chan os.Signal, 1)
	signal.Notify(sigint, os.Interrupt, syscall.SIGTERM)

	<-sigint

	printInfo("received a signal of graceful shutdown")

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err := sps.Shutdown(ctx)
	if err != nil {
		printError("failed to graceful shutdown", err)
		return
	}
	printInfo("completed graceful shutdown")

}

func getSpotifyTokenFromFile(dir string) (*oauth2.Token, error) {
	blob, err := ioutil.ReadFile(filepath.Join(dir, "spotify"))
	if err != nil {
		return nil, err
	}
	var token oauth2.Token
	err = json.Unmarshal(blob, &token)
	if err != nil {
		return nil, err
	}
	if token.Expiry.Before(time.Now()) {
		return nil, errors.New("expired token")
	}
	return &token, nil
}

func saveSpotifyTokenToFile(dir string, token *oauth2.Token) error {
	blob, err := json.Marshal(token)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(filepath.Join(dir, "spotify"), blob, 0644)
}

func newTypetalk(clientID, clientSecret, scope string) *typetalk.Client {
	tokenSource := &source.TokenSource{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scope:        scope,
	}
	oc := oauth2.NewClient(context.Background(), tokenSource)
	return typetalk.NewClient(oc)
}

func newSpotify(auth *spotify.Authenticator, token *oauth2.Token) (*spotify.Client, error) {
	c := auth.NewClient(token)
	c.AutoRetry = true
	user, err := c.CurrentUser()
	if err != nil {
		return nil, err
	}
	// use the sc to make calls that require authorization
	printDebug("You are logged in as:", user.ID)
	return &c, nil
}

type handler struct {
	tc              *typetalk.Client
	spaceKey, emoji string
}

func (h *handler) Serve(playing *spotify.CurrentlyPlaying) {
	// eg. https://open.spotify.com/track/6aOaB0vl2ilHxRb23Wiazv
	externalURL := playing.Item.ExternalURLs["spotify"]
	// eg. Retarded
	trackName := playing.Item.Name
	// eg. KID FRESINO
	artistName := playing.Item.Artists[0].Name
	// eg. Retarded/KID FRESINO
	metadata := fmt.Sprintf("%s/%s", trackName, artistName)
	if 25 < utf8.RuneCountInString(metadata) {
		metadata = string([]rune(metadata)[:25]) + "…"
	}
	// eg. Retarded/KID FRESINO https://open.spotify.com/track/6aOaB0vl2ilHxRb23Wiazv
	msg := fmt.Sprintf("%s %s", metadata, externalURL)
	printDebug("NOW PLAYING", "-", msg)
	_, _, err := h.tc.Statuses.SaveUserStatus(context.Background(),
		h.spaceKey, h.emoji, &typetalk.SaveUserStatusOptions{
			Message:                msg,
			ClearAt:                "",
			IsNotificationDisabled: false,
		})
	if err != nil {
		printError(err)
	}
}

func completeAuth(w http.ResponseWriter, r *http.Request) {
	tok, err := auth.Token(state, r)
	if err != nil {
		http.Error(w, "Couldn't get token", http.StatusForbidden)
		printFatal(err)
	}
	if st := r.FormValue("state"); st != state {
		http.NotFound(w, r)
		printFatal("State mismatch: %s != %s\n", st, state)
	}
	// use the token to get an authenticated client
	c, err := newSpotify(&auth, tok)
	if err != nil {
		http.Error(w, "Couldn't connect spotify api", http.StatusForbidden)
		printFatal(err)
	}
	fmt.Fprintf(w, "Login completed. You can close this tab.")
	ch <- c
}

func openBrowser(url string) error {
	var err error
	switch runtime.GOOS {
	case "linux":
		err = exec.Command("xdg-open", url).Start()
	case "windows":
		err = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		err = exec.Command("open", url).Start()
	default:
		err = fmt.Errorf("unsupported platform")
	}
	return err
}

func exists(filename string) bool {
	_, err := os.Stat(filename)
	return err == nil
}

func getDotDir(home string) string {
	if dir, exist := os.LookupEnv("XDG_CONFIG_HOME"); dir != "" && exist {
		return filepath.Join(dir, cmdName)
	}
	return filepath.Join(home, "."+cmdName)
}

func printDebug(args ...interface{}) {
	if config.Debug {
		args = append([]interface{}{"[DEBUG]"}, args...)
		log.Println(args...)
	}
}

func printInfo(args ...interface{}) {
	args = append([]interface{}{"[INFO]"}, args...)
	log.Println(args...)
}

func printError(args ...interface{}) {
	args = append([]interface{}{"[ERROR]"}, args...)
	log.Println(args...)
}

func printFatal(args ...interface{}) {
	args = append([]interface{}{"[FATAL]"}, args...)
	log.Fatalln(args...)
}
