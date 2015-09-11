package backend

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/cenkalti/backoff"
	"github.com/pkg/sftp"
	"github.com/travis-ci/worker/config"
	workerctx "github.com/travis-ci/worker/context"
	"github.com/travis-ci/worker/metrics"
	"golang.org/x/crypto/ssh"
	"golang.org/x/net/context"
)

var (
	nonAlphaNumRegexp     = regexp.MustCompile(`[^a-zA-Z0-9_]+`)
	metricNameCleanRegexp = regexp.MustCompile(`[^A-Za-z0-9.:-_]+`)
	jupiterBrainHelp      = map[string]string{
		"ENDPOINT":            "[REQUIRED] url to Jupiter Brain server, including auth",
		"SSH_KEY_PATH":        "[REQUIRED] path to SSH key used to access job VMs",
		"SSH_KEY_PASSPHRASE":  "[REQUIRED] passphrase for SSH key given as SSH_KEY_PATH",
		"KEYCHAIN_PASSWORD":   "[REQUIRED] password used ... somehow",
		"IMAGE_ALIASES":       "comma-delimited strings used as stable names for images (default: \"\")",
		"IMAGE_ALIAS_{ALIAS}": "full name for a given alias given via IMAGE_ALIASES, where the alias form in the key is uppercased and normalized by replacing non-alphanumerics with _",
		"BOOT_POLL_SLEEP":     "sleep interval between polling server for instance status (default 3s)",
	}
)

const (
	wrapperSh = `#!/bin/bash

[[ $(uname) = Linux ]] && exec bash ~/build.sh

[[ -f ~/build.sh.exit ]] && rm ~/build.sh.exit

until nc 127.0.0.1 15782; do sleep 1; done

until [[ -f ~/build.sh.exit ]]; do sleep 1; done
exit $(cat ~/build.sh.exit)
`
)

func init() {
	Register("jupiterbrain", "Jupiter Brain", jupiterBrainHelp, newJupiterBrainProvider)
}

type jupiterBrainProvider struct {
	client           *http.Client
	baseURL          *url.URL
	imageAliases     map[string]string
	sshKeyPath       string
	sshKeyPassphrase string
	keychainPassword string
	bootPollSleep    time.Duration
}

type jupiterBrainInstance struct {
	payload  *jupiterBrainInstancePayload
	provider *jupiterBrainProvider
}

type jupiterBrainInstancePayload struct {
	ID          string   `json:"id"`
	IPAddresses []string `json:"ip-addresses"`
	State       string   `json:"state"`
	BaseImage   string   `json:"base-image,omitempty"`
	Type        string   `json:"type,omitempty"`
}

type jupiterBrainDataResponse struct {
	Data []*jupiterBrainInstancePayload `json:"data"`
}

func newJupiterBrainProvider(cfg *config.ProviderConfig) (Provider, error) {
	if !cfg.IsSet("ENDPOINT") {
		return nil, ErrMissingEndpointConfig
	}

	if !cfg.IsSet("IMAGE_ALIASES") {
		return nil, fmt.Errorf("expected IMAGE_ALIASES config key")
	}

	aliasNames := cfg.Get("IMAGE_ALIASES")

	baseURL, err := url.Parse(cfg.Get("ENDPOINT"))
	if err != nil {
		return nil, err
	}

	aliasNamesSlice := strings.Split(aliasNames, ",")

	imageAliases := make(map[string]string, len(aliasNamesSlice))

	for _, aliasName := range aliasNamesSlice {
		normalizedAliasName := strings.ToUpper(string(nonAlphaNumRegexp.ReplaceAll([]byte(aliasName), []byte("_"))))

		key := fmt.Sprintf("IMAGE_ALIAS_%s", normalizedAliasName)
		if !cfg.IsSet(key) {
			return nil, fmt.Errorf("expected image alias %q", aliasName)
		}

		imageAliases[aliasName] = cfg.Get(key)
	}

	if !cfg.IsSet("SSH_KEY_PATH") {
		return nil, fmt.Errorf("expected SSH_KEY_PATH config key")
	}

	sshKeyPath := cfg.Get("SSH_KEY_PATH")

	if !cfg.IsSet("SSH_KEY_PASSPHRASE") {
		return nil, fmt.Errorf("expected SSH_KEY_PASSPHRASE config key")
	}

	sshKeyPassphrase := cfg.Get("SSH_KEY_PASSPHRASE")

	if !cfg.IsSet("KEYCHAIN_PASSWORD") {
		return nil, fmt.Errorf("expected KEYCHAIN_PASSWORD config key")
	}

	keychainPassword := cfg.Get("KEYCHAIN_PASSWORD")

	bootPollSleep := 3 * time.Second
	if cfg.IsSet("BOOT_POLL_SLEEP") {
		si, err := time.ParseDuration(cfg.Get("BOOT_POLL_SLEEP"))
		if err != nil {
			return nil, err
		}
		bootPollSleep = si
	}

	return &jupiterBrainProvider{
		client:           http.DefaultClient,
		baseURL:          baseURL,
		imageAliases:     imageAliases,
		sshKeyPath:       sshKeyPath,
		sshKeyPassphrase: sshKeyPassphrase,
		keychainPassword: keychainPassword,
		bootPollSleep:    bootPollSleep,
	}, nil
}

func (p *jupiterBrainProvider) Start(ctx context.Context, startAttributes *StartAttributes) (Instance, error) {
	u, err := p.baseURL.Parse("instances")
	if err != nil {
		return nil, err
	}

	imageName := p.getImageName(startAttributes)

	if imageName == "" {
		return nil, fmt.Errorf("no image alias for %#v", startAttributes)
	}

	workerctx.LoggerFromContext(ctx).WithFields(logrus.Fields{
		"image_name": imageName,
		"osx_image":  startAttributes.OsxImage,
		"language":   startAttributes.Language,
		"dist":       startAttributes.Dist,
		"group":      startAttributes.Group,
		"os":         startAttributes.OS,
	}).Info("selected image name")

	startBooting := time.Now()

	bodyPayload := map[string]map[string]string{
		"data": {
			"type":       "instances",
			"base-image": imageName,
		},
	}

	jsonBody, err := json.Marshal(bodyPayload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", u.String(), bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/vnd.api+json")

	resp, err := p.httpDo(req)
	if err != nil {
		return nil, err
	}
	defer io.Copy(ioutil.Discard, resp.Body)
	defer resp.Body.Close()

	if c := resp.StatusCode; c < 200 || c >= 300 {
		body, _ := ioutil.ReadAll(resp.Body)
		return nil, fmt.Errorf("expected 2xx from Jupiter Brain API, got %d (error: %s)", c, body)
	}

	dataPayload := &jupiterBrainDataResponse{}
	err = json.NewDecoder(resp.Body).Decode(dataPayload)
	if err != nil {
		workerctx.LoggerFromContext(ctx).WithFields(logrus.Fields{
			"err":     err,
			"payload": dataPayload,
			"body":    resp.Body,
		}).Error("couldn't decode created payload")
		return nil, fmt.Errorf("couldn't decode created payload: %s", err)
	}

	payload := dataPayload.Data[0]

	instanceReady := make(chan *jupiterBrainInstancePayload, 1)
	errChan := make(chan error, 1)
	go func(id string) {
		u, err := p.baseURL.Parse(fmt.Sprintf("instances/%s", url.QueryEscape(id)))
		if err != nil {
			errChan <- err
			return
		}

		req, err := http.NewRequest("GET", u.String(), nil)
		if err != nil {
			errChan <- err
			return
		}

		for {
			resp, err := p.httpDo(req)
			if err != nil {
				errChan <- err
				return
			}

			if resp.StatusCode != 200 {
				body, _ := ioutil.ReadAll(resp.Body)
				errChan <- fmt.Errorf("unknown status code: %d, expected 200 (body: %q)", resp.StatusCode, string(body))
				return
			}

			dataPayload := &jupiterBrainDataResponse{}
			err = json.NewDecoder(resp.Body).Decode(dataPayload)
			if err != nil {
				errChan <- fmt.Errorf("couldn't decode refresh payload: %s", err)
				return
			}
			payload := dataPayload.Data[0]

			_, _ = io.Copy(ioutil.Discard, resp.Body)
			_ = resp.Body.Close()

			var ip net.IP
			for _, ipString := range payload.IPAddresses {
				curIP := net.ParseIP(ipString)
				if curIP.To4() != nil {
					ip = curIP
					break
				}

			}

			if ip == nil {
				time.Sleep(p.bootPollSleep)
				continue
			}

			conn, err := net.Dial("tcp", fmt.Sprintf("%s:22", ip.String()))
			if conn != nil {
				conn.Close()
			}

			if err == nil {
				instanceReady <- payload
				return
			}

			time.Sleep(p.bootPollSleep)
		}
	}(payload.ID)

	select {
	case payload := <-instanceReady:
		metrics.TimeSince("worker.vm.provider.jupiterbrain.boot", startBooting)
		normalizedImageName := string(metricNameCleanRegexp.ReplaceAll([]byte(imageName), []byte("-")))
		metrics.TimeSince(fmt.Sprintf("worker.vm.provider.jupiterbrain.boot.image.%s", normalizedImageName), startBooting)
		workerctx.LoggerFromContext(ctx).WithField("instance_uuid", payload.ID).Info("booted instance")
		return &jupiterBrainInstance{
			payload:  payload,
			provider: p,
		}, nil
	case err := <-errChan:
		instance := &jupiterBrainInstance{
			payload:  payload,
			provider: p,
		}
		instance.Stop(ctx)

		return nil, err
	case <-ctx.Done():
		if ctx.Err() == context.DeadlineExceeded {
			metrics.Mark("worker.vm.provider.jupiterbrain.boot.timeout")
		}

		instance := &jupiterBrainInstance{
			payload:  payload,
			provider: p,
		}
		instance.Stop(ctx)

		return nil, ctx.Err()
	}
}

func (p *jupiterBrainProvider) Setup() error {
	return nil
}

func (p *jupiterBrainProvider) httpDo(req *http.Request) (*http.Response, error) {
	if req.URL.User != nil {
		token := req.URL.User.Username()
		req.URL.User = nil
		req.Header.Set("Authorization", "token "+token)
	}

	var resp *http.Response = nil

	b := backoff.NewExponentialBackOff()
	b.MaxInterval = 10 * time.Second
	b.MaxElapsedTime = time.Minute

	err := backoff.Retry(func() (err error) {
		resp, err = p.client.Do(req)
		return
	}, b)

	return resp, err
}

func (i *jupiterBrainInstance) UploadScript(ctx context.Context, script []byte) error {
	client, err := i.sshClient()
	if err != nil {
		return err
	}
	defer client.Close()

	sftp, err := sftp.NewClient(client)
	if err != nil {
		return err
	}
	defer sftp.Close()

	_, err = sftp.Lstat("build.sh")
	if err == nil {
		return ErrStaleVM
	}

	f, err := sftp.Create("build.sh")
	if err != nil {
		return err
	}

	_, err = f.Write(script)
	if err != nil {
		return err
	}

	f, err = sftp.Create("wrapper.sh")
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(f, wrapperSh)

	return err
}

func (i *jupiterBrainInstance) RunScript(ctx context.Context, output io.Writer) (*RunResult, error) {
	client, err := i.sshClient()
	if err != nil {
		return &RunResult{Completed: false}, err
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return &RunResult{Completed: false}, err
	}
	defer session.Close()

	err = session.RequestPty("xterm", 80, 40, ssh.TerminalModes{})
	if err != nil {
		return &RunResult{Completed: false}, err
	}

	session.Stdout = output
	session.Stderr = output

	errChan := make(chan error)

	go func() {
		errChan <- session.Run("bash ~/wrapper.sh")
	}()

	select {
	case <-ctx.Done():
		return &RunResult{Completed: false}, ctx.Err()
	case err = <-errChan:
	}

	if err == nil {
		return &RunResult{Completed: true, ExitCode: 0}, nil
	}

	switch err := err.(type) {
	case *ssh.ExitError:
		return &RunResult{Completed: true, ExitCode: uint8(err.ExitStatus())}, nil
	default:
		return &RunResult{Completed: false}, err
	}
}

func (i *jupiterBrainInstance) Stop(ctx context.Context) error {
	u, err := i.provider.baseURL.Parse(fmt.Sprintf("instances/%s", url.QueryEscape(i.payload.ID)))
	if err != nil {
		return err
	}

	req, err := http.NewRequest("DELETE", u.String(), nil)
	if err != nil {
		return err
	}

	resp, err := i.provider.httpDo(req)
	io.Copy(ioutil.Discard, resp.Body)
	resp.Body.Close()
	return err
}

func (i *jupiterBrainInstance) ID() string {
	if i.payload == nil {
		return "{unidentified}"
	}
	return fmt.Sprintf("%s:%s", i.payload.ID, i.payload.BaseImage)
}

func (i *jupiterBrainInstance) sshClient() (*ssh.Client, error) {
	file, err := ioutil.ReadFile(i.provider.sshKeyPath)
	if err != nil {
		return nil, err
	}

	block, _ := pem.Decode(file)
	if block == nil {
		return nil, fmt.Errorf("ssh key does not contain a valid PEM block")
	}

	der, err := x509.DecryptPEMBlock(block, []byte(i.provider.sshKeyPassphrase))
	if err != nil {
		return nil, err
	}

	key, err := x509.ParsePKCS1PrivateKey(der)
	if err != nil {
		return nil, err
	}

	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return nil, err
	}

	var ip net.IP
	for _, ipString := range i.payload.IPAddresses {
		curIP := net.ParseIP(ipString)
		if curIP.To4() != nil {
			ip = curIP
			break
		}

	}

	if ip == nil {
		return nil, fmt.Errorf("no valid IPv4 address")
	}

	return ssh.Dial("tcp", fmt.Sprintf("%s:22", ip.String()), &ssh.ClientConfig{
		User: "travis",
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
	})
}

func (p *jupiterBrainProvider) getImageName(startAttributes *StartAttributes) string {
	for _, key := range []string{
		startAttributes.OsxImage,
		fmt.Sprintf("osx_image_%s", startAttributes.OsxImage),
		fmt.Sprintf("osx_image_%s_%s", startAttributes.OsxImage, startAttributes.Language),
		fmt.Sprintf("dist_%s_%s", startAttributes.Dist, startAttributes.Language),
		fmt.Sprintf("dist_%s", startAttributes.Dist),
		fmt.Sprintf("group_%s_%s", startAttributes.Group, startAttributes.Language),
		fmt.Sprintf("group_%s", startAttributes.Group),
		fmt.Sprintf("language_%s", startAttributes.Language),
		fmt.Sprintf("default_%s", startAttributes.OS),
	} {
		imageName, ok := p.imageAliases[key]
		if ok {
			return imageName
		}
	}

	return ""
}
