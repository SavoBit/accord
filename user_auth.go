package accord

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/pkg/errors"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	google_oauth "google.golang.org/api/oauth2/v2"
)

// this is used for webflow authentication
// this ClientID is public so that the server can validate the access token
// to have been generated by the same clientID
// this should be added at compile time by the build system when the binary is made
var defaultClientID = "{{ClientID to be filled in at build time}}"
var defaultClientSecret = "{{ Client Secret to be filled in at build time}}"
var ClientID = defaultClientID
var ClientSecret = defaultClientSecret
var singleQuote = `'`
var scopes = []string{google_oauth.UserinfoEmailScope,
	google_oauth.UserinfoProfileScope}

// this only does Google Auth because that's what we need right now
// I don't think it's trivial to add other authentication protocols
// so until we need to, I'm sticking with Google Auth
type GoogleAuth struct {
	// Whether to use a webserver
	UseWebServer bool
	// If we are listening to a webserver, what port
	WebServerPort int
	// Domain to restrict users to
	Domain string
	// Where to save the token
	TokenCachePath string

	Token *oauth2.Token
}

// openURL opens a browser window to the specified location.
// This code originally appeared at:
//   http://stackoverflow.com/questions/10377243/how-can-i-launch-a-process-that-is-not-a-file-in-go
func openURL(url string) error {
	var err error
	switch runtime.GOOS {
	case "linux":
		err = exec.Command("xdg-open", url).Start()
	case "windows":
		err = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		err = exec.Command("open", url).Start()
	default:
		err = fmt.Errorf("Cannot open URL %s on this platform", url)
	}
	return err
}

func defaultTokenCacheFile() (string, error) {
	usr, err := user.Current()
	if err != nil {
		return "", err
	}
	tokenCacheDir := filepath.Join(usr.HomeDir, ".credentials")
	os.MkdirAll(tokenCacheDir, 0700)
	return filepath.Join(tokenCacheDir,
		url.QueryEscape("accord-credentials.json")), err
}

func tokenFromFile(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	t := &oauth2.Token{}
	err = json.NewDecoder(f).Decode(t)
	defer f.Close()
	return t, err
}

// Validate the token to avoid the "Confused Deputy Problem"
// https://www.ietf.org/mail-archive/web/unbearable/current/msg00135.html
// Classic paper:
func (u *GoogleAuth) ValidateToken(ctx context.Context) (bool, string, error) {
	log.Println("validating token")
	var httpClient = &http.Client{}
	oauth2Service, err := google_oauth.New(httpClient)
	if err != nil {
		return false, "", errors.Wrapf(err, "Failed to create a token source")
	}
	tokenInfoCall := oauth2Service.Tokeninfo()
	tokenInfoCall.AccessToken(u.Token.AccessToken)
	tokenInfo, err := tokenInfoCall.Do()
	if err != nil {
		return false, "", err
	}
	if tokenInfo.IssuedTo != ClientID {
		return false, "", fmt.Errorf("Token was generated by invalid clientID")
	}
	if !tokenInfo.VerifiedEmail {
		return false, "", fmt.Errorf("Token was obtained with an invalid email")
	}
	return true, tokenInfo.Email, nil
}

func (u *GoogleAuth) saveToken(token *oauth2.Token) {
	u.Token = token
	fmt.Println("trying to save token")
	fmt.Printf("Saving credential file to: %s\n", u.TokenCachePath)
	f, err := os.OpenFile(u.TokenCachePath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		log.Fatalf("Unable to cache oauth token: %v", err)
	}
	defer f.Close()
	json.NewEncoder(f).Encode(token)
}

// getTokenFromPrompt uses Config to request a Token and prompts the user
// to enter the token on the command line. It returns the retrieved Token.
func (u *GoogleAuth) getTokenFromPrompt(config *oauth2.Config, authURL string) (*oauth2.Token, error) {
	var code string
	fmt.Printf("Go to the following link in your browser. After completing "+
		"the authorization flow, enter the authorization code on the command "+
		"line: \n%v\n", authURL)

	if _, err := fmt.Scan(&code); err != nil {
		log.Fatalf("Unable to read authorization code %v", err)
	}
	fmt.Println(authURL)
	return exchangeToken(config, code)
}

func (u *GoogleAuth) getTokenFromWeb(config *oauth2.Config, authURL string) (*oauth2.Token, error) {
	codeCh, err := u.startWebServer()
	if err != nil {
		fmt.Printf("Unable to start a web server.")
		return nil, err
	}

	err = openURL(authURL)
	if err != nil {
		log.Fatalf("Unable to open authorization URL in web server: %v", err)
	} else {
		fmt.Println("Your browser has been opened to an authorization URL.",
			" This program will resume once authorization has been provided.")
		fmt.Println(authURL)
	}

	// Wait for the web server to get the code.
	code := <-codeCh
	return exchangeToken(config, code)
}

// Exchange the authorization code for an access token
func exchangeToken(config *oauth2.Config, code string) (*oauth2.Token, error) {
	// Use the custom HTTP client when requesting a token.
	//httpClient := &http.Client{Timeout: 2 * time.Second}
	//ctx := context.Background()
	//context.WithValue(ctx, oauth2.HTTPClient, httpClient)
	tok, err := config.Exchange(context.TODO(), code)
	if err != nil {
		return nil, errors.Wrapf(err, "Unable to retrieve token")
	}
	return tok, nil
}

// startWebServer starts a web server that listens on http://localhost:8080.
// The webserver waits for an oauth code in the three-legged auth flow.
func (u *GoogleAuth) startWebServer() (codeCh chan string, err error) {
	listener, err := net.Listen("tcp", "localhost:"+strconv.Itoa(u.WebServerPort))
	if err != nil {
		return nil, err
	}
	codeCh = make(chan string, 1)

	go http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code := r.FormValue("code")
		codeCh <- code // send code to OAuth flow
		listener.Close()
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "Received code: %v\r\nYou can now safely close this browser window.", code)
	}))

	return codeCh, nil
}

func (u *GoogleAuth) Authenticate() (*oauth2.Token, error) {
	if u.UseWebServer {
		if u.WebServerPort == 0 {
			u.WebServerPort = 8090
		}
	}
	if u.TokenCachePath == "" {
		tokenCachePath, err := defaultTokenCacheFile()
		if err != nil {
			return nil, errors.Wrapf(err, "Unable to get path to cached credential file. %s", u.TokenCachePath)
		}
		u.TokenCachePath = tokenCachePath
	}

	var (
		clientId     string
		clientSecret string
	)

	if ClientID == defaultClientID || ClientSecret == defaultClientSecret {
		return nil, errors.New("ClientID or Secret isn't set, both need to be set to continue authenticating. Get the pre-built version with the keys already set or get new one from Google Cloud and build")
	}

	// if being supplied at link time, the strings are wrapped with single quotes, in which case, remove them
	if string(ClientID[0]) == singleQuote {
		clientId = ClientID[1 : len(ClientID)-1]
	} else {
		clientId = ClientID
	}

	if string(ClientSecret[0]) == `'` {
		clientSecret = ClientSecret[1 : len(ClientSecret)-1]
	} else {
		clientSecret = ClientSecret
	}

	var config = &oauth2.Config{
		ClientID:     clientId,     // from https://console.developers.google.com/project/<your-project-id>/apiui/credential
		ClientSecret: clientSecret, // from https://console.developers.google.com/project/<your-project-id>/apiui/credential
		Endpoint:     google.Endpoint,
		Scopes:       scopes,
	}

	if u.UseWebServer {
		config.RedirectURL = "http://localhost:" + strconv.Itoa(u.WebServerPort)
	} else {
		config.RedirectURL = "urn:ietf:wg:oauth:2.0:oob"
	}

	var login = func() (tok *oauth2.Token, err error) {
		authURL := config.AuthCodeURL("state", oauth2.AccessTypeOffline,
			oauth2.SetAuthURLParam("hd", u.Domain))
		if !u.UseWebServer {
			tok, err = u.getTokenFromPrompt(config, authURL)
		} else {
			tok, err = u.getTokenFromWeb(config, authURL)
		}

		if err == nil {
			u.saveToken(tok)
		}
		return
	}
	tok, err := tokenFromFile(u.TokenCachePath)
	if err != nil {
		tok, err = login()
	}
	if !tok.Valid() {
		log.Println("Token is no longer valid, refreshing")
		// the refresh_token didn't quite work for my cases
		// since this isn't applicable to continuously requesting
		// to google services, using the client to auto refresh
		// isn't ideal. So re-logging in if the token isn't valid anymore
		tok, err = login()
	}
	return tok, err
}
