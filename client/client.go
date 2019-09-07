// Package client implements the DASH client
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"runtime"
	"time"

	"github.com/m-lab/ndt7-client-go/mlabns"
	"github.com/neubot/dash/common"
	"github.com/neubot/dash/internal"
)

const (
	// libraryName is the name of this library
	libraryName = "neubot-dash"

	// libraryVersion is the version of this library.
	libraryVersion = "0.1.0"

	// magicVersion is a magic number that identifies in a unique
	// way this implementation of DASH. 0.007xxxyyy is Measurement
	// Kit. Values lower than that are Neubot.
	magicVersion = "0.008000000"
)

var (
	// ErrTryAgain is returned when the Neubot server is busy.
	ErrTryAgain = errors.New("Server busy; try again later")

	// errHTTPRequestFailed is returned when an HTTP request fails.
	errHTTPRequestFailed = errors.New("HTTP request failed")
)

// Client is a DASH client
type Client struct {
	// ClientName is the name of the client application. This field is
	// initialized by the NewClient constructor.
	ClientName string

	// ClientVersion is the version of the client application. This field is
	// initialized by the NewClient constructor.
	ClientVersion string

	// FQDN is the server of the server to use. If the FQDN is not
	// specified, we'll use mlab-ns to discover a server.
	FQDN string

	// HTTPClient is the HTTP client used by this implementation. This field
	// is initialized by the NewClient to http.DefaultClient.
	HTTPClient *http.Client

	// Logger is the logger to use. This field is initialized by the
	// NewClient constructor to a do-nothing logger.
	Logger common.Logger

	// NumIterations is the num of iterations you want to perform.
	NumIterations int64

	// MLabNSClient is the mlabns client. We'll configure it with
	// defaults in NewClient and you may override it.
	MLabNSClient *mlabns.Client

	begin         time.Time
	clientResults []common.ClientResults
	err           error
	scheme        string
	serverResults []common.ServerResults
	userAgent     string
}

func makeUserAgent(clientName, clientVersion string) string {
	return clientName + "/" + clientVersion + " " + libraryName + "/" + libraryVersion
}

// NewClient creates a new client instance using the specified
// client application name and version.
func NewClient(clientName, clientVersion string) *Client {
	ua := makeUserAgent(clientName, clientVersion)
	return &Client{
		ClientName:    clientName,
		ClientVersion: clientVersion,
		HTTPClient:    http.DefaultClient,
		Logger:        internal.NoLogger{},
		MLabNSClient:  mlabns.NewClient("neubot", ua),
		NumIterations: 15,
		begin:         time.Now(),
		scheme:        "http",
		userAgent:     ua,
	}
}

// negotiate is the preliminary phase of Neubot experiment where we connect
// to the server, negotiate test parameters, and obtain an authorization
// token that will be used by us and by the server to identify this experiment.
func (c *Client) negotiate(ctx context.Context) (common.NegotiateResponse, error) {
	var negotiateResponse common.NegotiateResponse
	data, err := json.Marshal(common.NegotiateRequest{
		DASHRates: common.DefaultRates,
	})
	if err != nil {
		return negotiateResponse, err
	}
	c.Logger.Debugf("dash: body: %s", string(data))
	var URL url.URL
	URL.Scheme = c.scheme
	URL.Host = c.FQDN
	URL.Path = common.NegotiatePath
	req, err := http.NewRequest("POST", URL.String(), bytes.NewReader(data))
	if err != nil {
		return negotiateResponse, err
	}
	c.Logger.Debugf("dash: POST %s", URL.String())
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "")
	req = req.WithContext(ctx)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return negotiateResponse, err
	}
	c.Logger.Debugf("dash: StatusCode: %d", resp.StatusCode)
	if resp.StatusCode != 200 {
		return negotiateResponse, errHTTPRequestFailed
	}
	defer resp.Body.Close()
	data, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		return negotiateResponse, err
	}
	c.Logger.Debugf("dash: body: %s", string(data))
	err = json.Unmarshal(data, &negotiateResponse)
	if err != nil {
		return negotiateResponse, err
	}
	// Implementation oddity: Neubot is using an integer rather than a
	// boolean for the unchoked, with obvious semantics. I wonder why
	// I choose an integer over a boolean, given that Python does have
	// support for booleans. I don't remember 🤷.
	if negotiateResponse.Authorization == "" || negotiateResponse.Unchoked == 0 {
		return negotiateResponse, ErrTryAgain
	}
	c.Logger.Debugf("dash: authorization: %s", negotiateResponse.Authorization)
	return negotiateResponse, nil
}

// download implements the DASH test proper. We compute the number of bytes
// to request given the current rate, download the fake DASH segment, and
// then we return the measured performance of this segment to the caller. This
// is repeated several times to emulate downloading part of a video.
func (c *Client) download(
	ctx context.Context, authorization string, current *common.ClientResults,
) error {
	numBytes := (current.Rate * 1000 * current.ElapsedTarget) >> 3
	var URL url.URL
	URL.Scheme = c.scheme
	URL.Host = c.FQDN
	URL.Path = fmt.Sprintf("%s%d", common.DownloadPath, numBytes)
	req, err := http.NewRequest("GET", URL.String(), nil)
	if err != nil {
		return err
	}
	c.Logger.Debugf("dash: GET %s", URL.String())
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Authorization", authorization)
	req = req.WithContext(ctx)
	savedTicks := time.Now()
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	c.Logger.Debugf("dash: StatusCode: %d", resp.StatusCode)
	if resp.StatusCode != 200 {
		return errHTTPRequestFailed
	}
	defer resp.Body.Close()
	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	// Implementation note: MK contains a comment that says that Neubot uses
	// the elapsed time since when we start receiving the response but it
	// turns out that Neubot and MK do the same. So, we do what they do. At
	// the same time, we are currently not able to include the overhead that
	// is caused by HTTP headers etc. So, we're a bit less precise.
	current.Elapsed = time.Now().Sub(savedTicks).Seconds()
	current.Received = int64(len(data))
	current.RequestTicks = savedTicks.Sub(c.begin).Seconds()
	current.Timestamp = time.Now().Unix()
	//c.Logger.Debugf("dash: current: %+v", current) /* for debugging */
	return nil
}

// collect is the final phase of the test. We send to the server what we
// measured and we receive back what it has measured.
func (c *Client) collect(ctx context.Context, authorization string) error {
	data, err := json.Marshal(c.clientResults)
	if err != nil {
		return err
	}
	c.Logger.Debugf("dash: body: %s", string(data))
	var URL url.URL
	URL.Scheme = c.scheme
	URL.Host = c.FQDN
	URL.Path = common.CollectPath
	req, err := http.NewRequest("POST", URL.String(), bytes.NewReader(data))
	if err != nil {
		return err
	}
	c.Logger.Debugf("dash: POST %s", URL.String())
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authorization)
	req = req.WithContext(ctx)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	c.Logger.Debugf("dash: StatusCode: %d", resp.StatusCode)
	if resp.StatusCode != 200 {
		return errHTTPRequestFailed
	}
	defer resp.Body.Close()
	data, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	c.Logger.Debugf("dash: body: %s", string(data))
	err = json.Unmarshal(data, &c.serverResults)
	if err != nil {
		return err
	}
	return nil
}

// loop is the main loop of the DASH test. It performs negotiation, the test
// proper, and then collection. It posts interim results on |ch|.
func (c *Client) loop(ctx context.Context, ch chan<- common.ClientResults) {
	defer close(ch)
	// Implementation note: we will soon refactor the server to eliminate the
	// possiblity of keeping clients in queue. For this reason it's becoming
	// increasingly less important to loop waiting for the ready signal. Hence
	// if the server is busy, we just return a well known error.
	var negotiateResponse common.NegotiateResponse
	negotiateResponse, c.err = c.negotiate(ctx)
	if c.err != nil {
		return
	}
	// Note: according to a comment in MK sources 3000 kbit/s was the
	// minimum speed recommended by Netflix for SD quality in 2017.
	//
	// See: <https://help.netflix.com/en/node/306>.
	const initialBitrate = 3000
	current := common.ClientResults{
		ElapsedTarget: 2,
		Platform:      runtime.GOOS,
		Rate:          initialBitrate,
		RealAddress:   negotiateResponse.RealAddress,
		ServerURL:     "http://" + c.FQDN + "/",
		Version:       magicVersion,
	}
	for current.Iteration < c.NumIterations {
		c.err = c.download(ctx, negotiateResponse.Authorization, &current)
		if c.err != nil {
			return
		}
		c.clientResults = append(c.clientResults, current)
		ch <- current
		current.Iteration++
		speed := float64(current.Received) / float64(current.Elapsed)
		speed *= 8.0    // to bits per second
		speed /= 1000.0 // to kbit/s
		current.Rate = int64(speed)
	}
	c.err = c.collect(ctx, negotiateResponse.Authorization)
}

// StartDownload starts the DASH download. It returns a channel where
// interim measurements are posted, or an error. This function will only
// technically fail if we cannot even initiate the experiment. If you
// se some results on the returned channel, then maybe it means the
// experiment has somehow worked. You can see if there has been some
// error during the experiment by using Error().
func (c *Client) StartDownload(ctx context.Context) (<-chan common.ClientResults, error) {
	if c.FQDN == "" {
		c.Logger.Debug("dash: discovering server with mlabns")
		fqdn, err := c.MLabNSClient.Query(ctx)
		if err != nil {
			return nil, err
		}
		c.FQDN = fqdn
	}
	c.Logger.Debugf("dash: using server: %s", c.FQDN)
	ch := make(chan common.ClientResults)
	go c.loop(ctx, ch)
	return ch, nil
}

// Error returns the error that occurred during the test, if any. A nil
// return value means that all was good. A returned error does not however
// necessarily mean that all was bad; you may have _some_ data.
func (c *Client) Error() error {
	return c.err
}

// ServerResults returns the results of the experiment collected by the
// server. In case Error() returns non nil, this function will typically
// return an empty slice to the caller.
func (c *Client) ServerResults() []common.ServerResults {
	return c.serverResults
}
