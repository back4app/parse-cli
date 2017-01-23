package parsecli

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bgentry/go-netrc/netrc"
	"github.com/bgentry/speakeasy"
	"github.com/facebookgo/jsonpipe"
	"github.com/facebookgo/stackerr"
	"github.com/mitchellh/go-homedir"
	"github.com/skratchdot/open-golang/open"
)

const keysURL = "http://dashboard.back4app.com/classic#/wizard/account-key"

type Credentials struct {
	Email    string
	Password string
	Token    string
}

type Login struct {
	Credentials Credentials
	TokenReader io.Reader
}

var (
	errAuth = errors.New(`Sorry, we do not have a user with this username and password.
If you do not remember your password, please follow instructions at:
  http://back4app.com/password/forgot
to reset your password`)

	tokenErrMsgf = `Sorry, the account key: %q you provided is not valid.
Please follow instructions at %q to generate a new one.
`
	keyNotConfigured = regexp.MustCompile("Account key not configured")

	parseNetrc = filepath.Join(".back4app", "netrc")
)

func accountKeyNotConfigured(err error) bool {
	if err == nil {
		return false
	}
	return keyNotConfigured.MatchString(err.Error())
}

func (l *Login) populateCreds(e *Env) error {
	if l.Credentials.Email != "" && l.Credentials.Password != "" {
		return nil
	}

	fmt.Fprint(e.Out, "Email: ")
	fmt.Fscanf(e.In, "%s\n", &l.Credentials.Email)

	var (
		password string
		err      error
	)
	if e.In == os.Stdin {
		password, err = speakeasy.Ask("Password (will be hidden): ")
		if err != nil {
			return err
		}
	} else {
		// NOTE: only for testing
		fmt.Fscanf(e.In, "%s\n", &password)
	}

	if password != "" {
		l.Credentials.Password = password
	}

	return nil
}

func (l *Login) getTokensReader() (io.Reader, error) {
	if l.TokenReader != nil {
		return l.TokenReader, nil
	}
	homeDir, err := homedir.Dir()
	if err != nil {
		return nil, stackerr.Wrap(err)
	}
	location := filepath.Join(homeDir, parseNetrc)
	file, err := os.OpenFile(location, os.O_RDONLY, 0600)
	if err != nil {
		return nil, stackerr.Wrap(err)
	}
	l.TokenReader = file
	return file, nil
}

func (l *Login) GetTokenCredentials(e *Env, email string) (bool, *Credentials, error) {
	reader, err := l.getTokensReader()
	if err != nil {
		return false, nil, stackerr.Wrap(err)
	}
	tokens, err := netrc.Parse(reader)
	if err != nil {
		return false, nil, stackerr.Wrap(err)
	}
	server, err := getHostFromURL(e.Server, email)
	if err != nil {
		return false, nil, err
	}
	machine := tokens.FindMachine(server)
	if machine != nil {
		return true,
			&Credentials{
				Token: machine.Password,
			}, nil
	}

	if email == "" {
		return false, nil, stackerr.Newf("Could not find account key for %q", server)
	}

	// check for system default account key for the given server
	// since we could not find account key for the given account (email)
	server, err = getHostFromURL(e.Server, "")
	if err != nil {
		return false, nil, err
	}
	machine = tokens.FindMachine(server)
	if machine != nil {
		return false,
			&Credentials{
				Token: machine.Password,
			}, nil
	}
	return false,
		nil,
		stackerr.Newf(
			`Could not find account key for email: %q,
and default access key not configured for %q
`,
			email,
			e.Server,
		)
}

func (l *Login) updatedNetrcContent(
	e *Env,
	content io.Reader,
	email string,
	credentials *Credentials,
) ([]byte, error) {
	tokens, err := netrc.Parse(content)
	if err != nil {
		return nil, stackerr.Wrap(err)
	}

	server, err := getHostFromURL(e.Server, email)
	if err != nil {
		return nil, err
	}
	machine := tokens.FindMachine(server)
	if machine == nil {
		machine = tokens.NewMachine(server, "default", credentials.Token, "")
	} else {
		machine.UpdatePassword(credentials.Token)
	}

	updatedContent, err := tokens.MarshalText()
	if err != nil {
		return nil, stackerr.Wrap(err)
	}
	return updatedContent, nil
}

func (l *Login) StoreCredentials(e *Env, email string, credentials *Credentials) error {
	if l.TokenReader != nil {
		// tests should not store credentials
		return nil
	}

	homeDir, err := homedir.Dir()
	if err != nil {
		return stackerr.Wrap(err)
	}

	location := filepath.Join(homeDir, parseNetrc)
	if err := os.MkdirAll(filepath.Dir(location), 0755); err != nil {
		return stackerr.Wrap(err)
	}
	file, err := os.OpenFile(location, os.O_RDONLY|os.O_CREATE, 0600)
	if err != nil {
		return stackerr.Wrap(err)
	}
	content, err := l.updatedNetrcContent(e, file, email, credentials)
	if err != nil {
		return err
	}

	file, err = os.OpenFile(location, os.O_WRONLY|os.O_TRUNC, 0600)
	_, err = file.Write(content)
	return stackerr.Wrap(err)
}

func (l *Login) AuthToken(e *Env, token string) (string, error) {
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "accountkey"},
		Body: ioutil.NopCloser(
			jsonpipe.Encode(
				map[string]string{
					"accountKey": token,
				},
			),
		),
	}

	res := &struct {
		Email string `json:"email"`
	}{}
	if response, err := e.ParseAPIClient.Do(req, nil, res); err != nil {
		if response != nil && response.StatusCode == http.StatusUnauthorized {
			return "", stackerr.Newf(tokenErrMsgf, Last4(token), keysURL)
		}
		return "", stackerr.Wrap(err)
	}

	if e.ParserEmail != "" && res.Email != e.ParserEmail {
		return "", stackerr.Newf("Account key %q does not belong to %q", Last4(token), e.ParserEmail)
	}
	return res.Email, nil
}

func (l *Login) AuthUserWithToken(e *Env, strict bool) (string, error) {
	_, tokenCredentials, err := l.GetTokenCredentials(e, e.ParserEmail)
	if err != nil {
		// user never created an account key: educate them
		if stackerr.HasUnderlying(err, stackerr.MatcherFunc(os.IsNotExist)) {
			if strict {
				fmt.Fprintln(
					e.Out,
					`To proceed further, you must configure an account key.
`,
				)
			} else {
				fmt.Fprintln(
					e.Out,
					`We've changed the way the CLI works.
To save time logging in, you should create an account key.
`,
				)

			}

			fmt.Fprintln(
				e.Out,
				`Type "b4a configure accountkey" to create a new account key.
Read more at: http://docs.back4app.com/docs/integrations/command-line-interface/account-keys/`)
			return "", stackerr.New("Account key not configured")
		}

		return "", err
	}

	email, err := l.AuthToken(e, tokenCredentials.Token)
	if err != nil {
		fmt.Fprintf(e.Err, "Account key could not be used.\nError: %s\n\n", ErrorString(e, err))
		return "", err
	}

	l.Credentials = *tokenCredentials
	return email, nil
}

func (l *Login) AuthUser(e *Env, strict bool) error {
	_, err := l.AuthUserWithToken(e, strict)
	if err == nil {
		return nil
	}
	if strict {
		return err
	}

	if !stackerr.HasUnderlying(err, stackerr.MatcherFunc(accountKeyNotConfigured)) {
		fmt.Fprintln(
			e.Out,
			`Type "b4a configure accountkey" to create a new account key.
Read more at: http://docs.back4app.com/docs/integrations/command-line-interface/account-keys/

Please login to Back4App using your email and password.`,
		)
	}

	apps := &Apps{}
	for i := 0; i < numRetries; i++ {
		err := l.populateCreds(e)
		if err != nil {
			return err
		}
		apps.Login.Credentials = l.Credentials

		_, err = apps.RestFetchApps(e)
		if err == nil {
			return nil
		}

		if i == numRetries-1 && err != nil {
			return err
		}
		if err != errAuth {
			fmt.Fprintf(e.Err, "Got error: %s", ErrorString(e, err))
		}
		fmt.Fprintf(e.Err, "%s\nPlease try again...\n", err)
		l.Credentials.Password = ""
	}
	return errAuth
}

func (l *Login) HelpCreateToken(e *Env) (string, error) {
	for i := 0; i < 4; i++ {
		fmt.Fprintf(e.Out, `
Input your account key or press ENTER to generate a new one.
NOTE: on pressing ENTER we'll try to open the url:
	%q
in default browser.
`,
			keysURL,
		)
		fmt.Fprintf(e.Out, `Account Key: `)

		var token string
		fmt.Fscanf(e.In, "%s\n", &token)
		token = strings.TrimSpace(token)
		if token != "" {
			return token, nil
		}

		err := open.Run(keysURL)
		if err != nil {
			fmt.Fprintf(e.Err,
				`Sorry, we couldn’t open the browser for you.
Go here to generate an account key: %q
`,
				keysURL,
			)
		}
	}
	return "", stackerr.New("Account key cannot be empty. Please try again.")
}
