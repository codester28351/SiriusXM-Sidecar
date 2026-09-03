// Package sxm is a Go port of the supplied sxm.py SiriusXM proxy.
//
// It intentionally keeps the same HTTP/API shape as the Python program:
// channel discovery, M3U/XSPF generation, playlist rewriting, segment
// retrieval, metadata injection, image proxying, and server-mode control.
package sxm

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	UserAgent      = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_12_6) AppleWebKit/604.5.6 (KHTML, like Gecko) Version/11.0.3 Safari/604.5.6"
	RestBase       = "https://player.siriusxm.com/rest/v2/experience/modules/"
	RestFormat     = RestBase + "%s"
	LivePrimaryHLS = "https://siriusxm-priprodlive.akamaized.net"
)

type SiriusXM struct {
	client       *http.Client
	username     string
	password     string
	playlists    map[string]string
	segmentURLs  map[string]string
	nowPlaying   map[string]NowPlaying
	forceChannel string
	channels     []Channel
	serverMode   bool
	mu           sync.RWMutex
}

type Channel struct {
	ChannelGuid      string      `json:"channelGuid"`
	ChannelID        string      `json:"channelId"`
	Name             string      `json:"name"`
	SiriusChannelNum interface{} `json:"siriusChannelNumber"`
	IsFavorite       bool        `json:"isFavorite"`
	Images           Images      `json:"images"`
}

type Images struct {
	Images []Image `json:"images"`
}

type Image struct {
	URL string `json:"url"`
}

type NowPlaying struct {
	Artists []Artist `json:"artists"`
	Title   string   `json:"title"`
	Extra map[string]json.RawMessage `json:"-"`
}

type Artist struct {
	Name string `json:"name"`
}

type apiResponse struct {
	ModuleListResponse struct {
		Status   int `json:"status"`
		Messages []struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
		} `json:"messages"`
		ModuleList struct {
			Modules []struct {
				ModuleResponse struct {
					LiveChannelData struct {
						HLSAudioInfos []struct {
							Size string `json:"size"`
							URL  string `json:"url"`
						} `json:"hlsAudioInfos"`
						MarkerLists []struct {
							Markers []struct {
								Cut NowPlaying `json:"cut"`
							} `json:"markers"`
						} `json:"markerLists"`
					} `json:"liveChannelData"`
					ContentData struct {
						ChannelListing struct {
							Channels []Channel `json:"channels"`
						} `json:"channelListing"`
					} `json:"contentData"`
				} `json:"moduleResponse"`
			} `json:"modules"`
		} `json:"moduleList"`
	} `json:"ModuleListResponse"`
}

func New(username, password string) *SiriusXM {
	jar, _ := cookiejar.New(nil)

	return &SiriusXM{
		client:      &http.Client{Jar: jar, Timeout: 30 * time.Second},
		username:    username,
		password:    password,
		playlists:   make(map[string]string),
		segmentURLs: make(map[string]string),
		nowPlaying:  make(map[string]NowPlaying),
	}
}

func (s *SiriusXM) logf(format string, args ...any) {
	log.Printf("<SiriusXM>: "+format, args...)
}

func (s *SiriusXM) isLoggedIn() bool {
	u, err := url.Parse(RestBase)
	if err != nil {
		return false
	}
	for _, c := range s.client.Jar.Cookies(u) {
		if c.Name == "SXMDATA" {
			return true
		}
	}
	return false
}

func (s *SiriusXM) isSessionAuthenticated() bool {
	u, err := url.Parse(RestBase)
	if err != nil {
		return false
	}
	hasAWS, hasJ := false, false
	for _, c := range s.client.Jar.Cookies(u) {
		if c.Name == "AWSALB" {
			hasAWS = true
		}
		if c.Name == "JSESSIONID" {
			hasJ = true
		}
	}
	return hasAWS && hasJ
}

func (s *SiriusXM) cookie(name string) string {
	u, err := url.Parse(RestBase)
	if err != nil {
		return ""
	}
	for _, c := range s.client.Jar.Cookies(u) {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

func (s *SiriusXM) sxmakToken() string {
	v := s.cookie("SXMAKTOKEN")
	if i := strings.IndexByte(v, '='); i >= 0 {
		v = v[i+1:]
	}
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return v
}

func (s *SiriusXM) gupID() string {
	v := s.cookie("SXMDATA")
	if v == "" {
		return ""
	}
	decoded, err := url.QueryUnescape(v)
	if err != nil {
		return ""
	}
	var x struct {
		GupID string `json:"gupId"`
	}
	if json.Unmarshal([]byte(decoded), &x) != nil {
		return ""
	}
	return x.GupID
}

func (s *SiriusXM) request(method, endpoint string, params url.Values, body []byte, authenticate bool) ([]byte, int, error) {
	if authenticate && !s.isSessionAuthenticated() && !s.authenticate() {
		return nil, 0, errors.New("unable to authenticate")
	}
	target := fmt.Sprintf(RestFormat, endpoint)
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, target, r)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode != http.StatusOK {
		return data, resp.StatusCode, nil
	}
	return data, resp.StatusCode, nil
}

func (s *SiriusXM) get(endpoint string, params url.Values, authenticate bool) (map[string]any, error) {
	data, status, err := s.request(http.MethodGet, endpoint, params, nil, authenticate)
	if err != nil {
		s.logf("%v", err)
		return nil, err
	}
	if status != 200 {
		s.logf("Received status code %d for method '%s'", status, endpoint)
		return nil, fmt.Errorf("HTTP %d", status)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		s.logf("Error decoding json for method '%s'", endpoint)
		return nil, err
	}
	return out, nil
}

func (s *SiriusXM) post(endpoint string, postdata any, authenticate bool) (map[string]any, error) {
	if authenticate && !s.isSessionAuthenticated() && !s.authenticate() {
		return nil, errors.New("unable to authenticate")
	}
	body, err := json.Marshal(postdata)
	if err != nil {
		return nil, err
	}
	data, status, err := s.request(http.MethodPost, endpoint, nil, body, false)
	if err != nil {
		s.logf("%v", err)
		return nil, err
	}
	if status != 200 {
		s.logf("Received status code %d for method '%s'", status, endpoint)
		return nil, fmt.Errorf("HTTP %d", status)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		s.logf("Error decoding json for method '%s'", endpoint)
		return nil, err
	}
	return out, nil
}

func (s *SiriusXM) login() bool {
	postdata := map[string]any{
		"moduleList": map[string]any{
			"modules": []any{
				map[string]any{
					"moduleRequest": map[string]any{
						"resultTemplate": "web",
						"deviceInfo": map[string]any{
							"osVersion": "Mac", "platform": "Web", "sxmAppVersion": "3.1802.10011.0",
							"browser": "Safari", "browserVersion": "11.0.3", "appRegion": "US",
							"deviceModel": "K2WebClient", "clientDeviceId": "null", "player": "html5", "clientDeviceType": "web",
						},
						"standardAuth": map[string]any{"username": s.username, "password": s.password},
					},
				},
			},
		},
	}
	data, err := s.post("modify/authentication", postdata, false)
	if err != nil || data == nil {
		return false
	}
	m, ok := data["ModuleListResponse"].(map[string]any)
	if !ok {
		return false
	}
	status, ok := m["status"].(float64)
	return ok && status == 1 && s.isLoggedIn()
}

func (s *SiriusXM) authenticate() bool {
	if !s.isLoggedIn() && !s.login() {
		s.logf("Unable to authenticate because login failed")
		return false
	}
	postdata := map[string]any{
		"moduleList": map[string]any{
			"modules": []any{
				map[string]any{
					"moduleRequest": map[string]any{
						"resultTemplate": "web",
						"deviceInfo": map[string]any{
							"osVersion": "Mac", "platform": "Web", "clientDeviceType": "web",
							"sxmAppVersion": "3.1802.10011.0", "browser": "Safari", "browserVersion": "11.0.3",
							"appRegion": "US", "deviceModel": "K2WebClient", "player": "html5", "clientDeviceId": "null",
						},
					},
				},
			},
		},
	}
	data, err := s.post("resume?OAtrial=false", postdata, false)
	if err != nil || data == nil {
		return false
	}
	m, ok := data["ModuleListResponse"].(map[string]any)
	if !ok {
		return false
	}
	status, ok := m["status"].(float64)
	return ok && status == 1 && s.isSessionAuthenticated()
}

func (s *SiriusXM) getPlaylistURL(guid, channelID string, useCache bool, attempts int) string {
	s.mu.RLock()
	cached := s.playlists[channelID]
	s.mu.RUnlock()
	if useCache && cached != "" {
		return cached
	}
	now := time.Now().UTC()
	params := url.Values{
		"assetGUID": {guid}, "ccRequestType": {"AUDIO_VIDEO"}, "channelId": {channelID},
		"hls_output_mode": {"custom"}, "marker_mode": {"all_separate_cue_points"}, "result-template": {"web"},
		"time": {strconv.FormatInt(time.Now().UnixMilli(), 10)}, "timestamp": {now.Format("2006-01-02T15:04:05.999999999Z07:00") + "Z"},
	}
	data, err := s.get("tune/now-playing-live", params, true)
	if err != nil || data == nil {
		return ""
	}
	root, ok := data["ModuleListResponse"].(map[string]any)
	if !ok {
		return ""
	}
	status, _ := root["status"].(float64)
	msgs, _ := root["messages"].([]any)
	code := 0
	msg := ""
	if len(msgs) > 0 {
		if m, ok := msgs[0].(map[string]any); ok {
			code = int(mustFloat(m["code"]))
			msg, _ = m["message"].(string)
		}
	}
	if code == 201 || code == 208 {
		if attempts > 0 && s.authenticate() {
			return s.getPlaylistURL(guid, channelID, useCache, attempts-1)
		}
		s.logf("Reached max attempts for playlist")
		return ""
	}
	if code != 100 || int(status) == 0 {
		s.logf("Received error %d %s", code, msg)
		return ""
	}
	ml, _ := root["moduleList"].(map[string]any)
	mods, _ := ml["modules"].([]any)
	if len(mods) == 0 {
		return ""
	}
	mod, _ := mods[0].(map[string]any)
	mr, _ := mod["moduleResponse"].(map[string]any)
	lcd, _ := mr["liveChannelData"].(map[string]any)
	infos, _ := lcd["hlsAudioInfos"].([]any)
	for _, raw := range infos {
		pi, _ := raw.(map[string]any)
		if pi["size"] != "LARGE" {
			continue
		}
		purl, _ := pi["url"].(string)
		purl = strings.Replace(purl, "%Live_Primary_HLS%", LivePrimaryHLS, 1)
		variant := s.getPlaylistVariantURL(purl)
		if variant == "" {
			continue
		}
		var np NowPlaying
		if lists, ok := lcd["markerLists"].([]any); ok && len(lists) > 0 {
			if l, ok := lists[len(lists)-1].(map[string]any); ok {
				if ms, ok := l["markers"].([]any); ok && len(ms) > 0 {
					if mm, ok := ms[len(ms)-1].(map[string]any); ok {
						if cut, ok := mm["cut"].(map[string]any); ok {
							b, _ := json.Marshal(cut)
							_ = json.Unmarshal(b, &np)
						}
					}
				}
			}
		}
		s.mu.Lock()
		s.playlists[channelID] = variant
		s.nowPlaying[channelID] = np
		s.mu.Unlock()
		return variant
	}
	return ""
}

func mustFloat(v any) float64 { f, _ := v.(float64); return f }

func (s *SiriusXM) getPlaylistVariantURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		s.logf("Invalid playlist URL: %v", err)
		return ""
	}

	q := u.Query()
	q.Set("token", s.sxmakToken())
	q.Set("consumer", "k2")
	q.Set("gupId", s.gupID())
	u.RawQuery = q.Encode()

	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		s.logf("Unable to create playlist request: %v", err)
		return ""
	}

	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/vnd.apple.mpegurl, application/x-mpegURL, */*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Referer", "https://player.siriusxm.com/")
	req.Header.Set("Origin", "https://player.siriusxm.com")

	res, err := s.client.Do(req)
	if err != nil {
		s.logf("Playlist request failed: %v", err)
		return ""
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusForbidden {
		s.logf("SiriusXM playlist returned 403")
		return ""
	}

	if res.StatusCode != http.StatusOK {
		s.logf("SiriusXM playlist returned HTTP %d", res.StatusCode)
		return ""
	}

	b, err := io.ReadAll(res.Body)
	if err != nil {
		return ""
	}

	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		ref, err := url.Parse(line)
		if err != nil {
			continue
		}

		resolved := u.ResolveReference(ref)

		vq := resolved.Query()
		vq.Set("token", s.sxmakToken())
		vq.Set("consumer", "k2")
		vq.Set("gupId", s.gupID())
		resolved.RawQuery = vq.Encode()

		if strings.HasSuffix(strings.ToLower(resolved.Path), ".m3u8") {
			return resolved.String()
		}
	}

	return ""
}

func (s *SiriusXM) GetPlaylist(name string, useCache bool) string {
	guid, cid, _, _, _ := s.GetChannel(name)

	if guid == "" || cid == "" {
		s.logf("No channel for %s", name)
		return ""
	}

	s.mu.Lock()
	if s.forceChannel == "" {
		s.forceChannel = cid
	}
	s.mu.Unlock()

	u := s.getPlaylistURL(guid, cid, useCache, 5)
	if u == "" {
		return ""
	}

	parsed, err := url.Parse(u)
	if err != nil {
		return ""
	}

	q := parsed.Query()
	q.Set("token", s.sxmakToken())
	q.Set("consumer", "k2")
	q.Set("gupId", s.gupID())
	parsed.RawQuery = q.Encode()

	req, err := http.NewRequest(http.MethodGet, parsed.String(), nil)
	if err != nil {
		return ""
	}

	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/vnd.apple.mpegurl, application/x-mpegURL, */*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Referer", "https://player.siriusxm.com/")
	req.Header.Set("Origin", "https://player.siriusxm.com")

	res, err := s.client.Do(req)
	if err != nil {
		s.logf("Playlist request failed: %v", err)
		return ""
	}

	data, readErr := io.ReadAll(res.Body)
	res.Body.Close()

	if readErr != nil {
		return ""
	}

	if res.StatusCode == http.StatusForbidden {
		s.logf("Playlist returned 403; refreshing SiriusXM session")

		if !s.authenticate() {
			s.logf("SiriusXM re-authentication failed")
			return ""
		}

		fresh := s.getPlaylistURL(guid, cid, false, 3)
		if fresh == "" {
			return ""
		}

		return s.GetPlaylist(name, false)
	}

	if res.StatusCode != http.StatusOK {
		s.logf("Playlist returned HTTP %d", res.StatusCode)
		return ""
	}

	baseURL := *parsed
	baseURL.Path = path.Dir(parsed.Path) + "/"
	baseURL.RawQuery = ""

	s.mu.RLock()
	np := s.nowPlaying[cid]
	s.mu.RUnlock()

	artist := ""
	title := ""

	if len(np.Artists) > 0 {
		artist = np.Artists[0].Name
	}

	title = np.Title

	var out []string

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")

		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "#EXTINF") {
			continue
		}

		if strings.HasPrefix(line, "#") {
			out = append(out, line)
			continue
		}

		ref, err := url.Parse(strings.TrimSpace(line))
		if err != nil {
			continue
		}

		segmentURL := baseURL.ResolveReference(ref)

		segmentQuery := segmentURL.Query()
		segmentQuery.Set("token", s.sxmakToken())
		segmentQuery.Set("consumer", "k2")
		segmentQuery.Set("gupId", s.gupID())
		segmentURL.RawQuery = segmentQuery.Encode()

		localName := path.Base(segmentURL.Path)

		if strings.HasSuffix(strings.ToLower(localName), ".aac") {
			localPath := "/" + cid + "/" + localName

			s.mu.Lock()
			s.segmentURLs[localPath] = segmentURL.String()
			s.mu.Unlock()

			out = append(
				out,
				fmt.Sprintf(
					"#EXTINF:10.0,%s - %s",
					artist,
					title,
				),
				localPath,
			)
		} else {
			out = append(out, segmentURL.String())
		}
	}

	return strings.Join(out, "\n")
}

func (s *SiriusXM) GetSegment(p string, attempts int) []byte {
	localPath := "/" + strings.TrimPrefix(p, "/")

	s.mu.RLock()
	target := s.segmentURLs[localPath]
	s.mu.RUnlock()

	if target == "" {
		target = LivePrimaryHLS + "/" + strings.TrimPrefix(p, "/")
	}

	fetch := func(targetURL string) ([]byte, int, error) {
		u, err := url.Parse(targetURL)
		if err != nil {
			return nil, 0, err
		}

		q := u.Query()
		q.Set("token", s.sxmakToken())
		q.Set("consumer", "k2")
		q.Set("gupId", s.gupID())
		u.RawQuery = q.Encode()

		req, err := http.NewRequest(http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, 0, err
		}

		req.Header.Set("User-Agent", UserAgent)
		req.Header.Set("Accept", "audio/aac,audio/*,*/*;q=0.8")
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")
		req.Header.Set("Referer", "https://player.siriusxm.com/")
		req.Header.Set("Origin", "https://player.siriusxm.com")
		req.Header.Set("Cache-Control", "no-cache")

		res, err := s.client.Do(req)
		if err != nil {
			return nil, 0, err
		}

		defer res.Body.Close()

		data, err := io.ReadAll(res.Body)
		if err != nil {
			return nil, res.StatusCode, err
		}

		return data, res.StatusCode, nil
	}

	data, status, err := fetch(target)
	if err != nil {
		s.logf("Segment request failed: %v", err)
		return nil
	}

	if status == http.StatusForbidden && attempts > 0 {
		s.logf("Segment returned 403; refreshing SiriusXM session")

		if !s.authenticate() {
			s.logf("SiriusXM re-authentication failed")
			return nil
		}

		parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
		if len(parts) >= 1 {
			channelID := parts[0]

			_, _, channelName, _, _ := s.GetChannel(channelID)

			if channelName != "" {
				s.GetPlaylist(channelName, false)
			}
		}

		return s.GetSegment(p, attempts-1)
	}

	if status != http.StatusOK {
		s.logf("SiriusXM segment returned HTTP %d", status)
		return nil
	}

	return data
}

func (s *SiriusXM) GetChannels() []Channel {
	s.mu.RLock()
	if len(s.channels) > 0 {
		c := append([]Channel(nil), s.channels...)
		s.mu.RUnlock()
		return c
	}
	s.mu.RUnlock()
	postdata := map[string]any{"moduleList": map[string]any{"modules": []any{map[string]any{"moduleArea": "Discovery", "moduleType": "ChannelListing", "moduleRequest": map[string]any{"consumeRequests": []any{}, "resultTemplate": "responsive", "alerts": []any{}, "profileInfos": []any{}}}}}}
	data, err := s.post("get", postdata, true)
	if err != nil {
		return nil
	}
	b, _ := json.Marshal(data)
	var ar apiResponse
	if json.Unmarshal(b, &ar) != nil {
		return nil
	}
	mods := ar.ModuleListResponse.ModuleList.Modules
	if len(mods) == 0 {
		return nil
	}
	c := mods[0].ModuleResponse.ContentData.ChannelListing.Channels
	s.mu.Lock()
	s.channels = c
	s.mu.Unlock()
	return c
}

func channelNum(c Channel) string {
	switch v := c.SiriusChannelNum.(type) {
	case string:
		return v
	case float64:
		return strconv.Itoa(int(v))
	default:
		return ""
	}
}
func imageURL(c Channel) string {
	if len(c.Images.Images) > 3 {
		return c.Images.Images[3].URL
	}
	return ""
}

func (s *SiriusXM) GetChannel(name string) (guid, cid, cname, logo, num string) {
	name = strings.ToLower(name)
	for _, c := range s.GetChannels() {
		n := channelNum(c)
		if strings.ToLower(c.Name) == name || strings.ToLower(c.ChannelID) == name || n == name {
			return c.ChannelGuid, c.ChannelID, c.Name, imageURL(c), n
		}
	}
	return
}
func (s *SiriusXM) SetServerMode(v bool) bool {
	s.mu.Lock()
	s.serverMode = v
	s.mu.Unlock()
	s.logf("Server mode set to %t", v)
	return v
}
func (s *SiriusXM) GetServerMode() bool { s.mu.RLock(); defer s.mu.RUnlock(); return s.serverMode }

func sortedChannels(ch []Channel) []Channel {
	out := append([]Channel(nil), ch...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IsFavorite != out[j].IsFavorite {
			return out[i].IsFavorite
		}
		a, _ := strconv.Atoi(channelNum(out[i]))
		b, _ := strconv.Atoi(channelNum(out[j]))
		return a < b
	})
	return out
}

func (s *SiriusXM) ChannelsToM3U() string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	for _, c := range sortedChannels(s.GetChannels()) {
		fmt.Fprintf(&b, "#EXTINF:-1 tvg-id=\"%s\" tvg-logo=\"%s\",%s %s\n/%s.m3u8\n", c.ChannelID, imageURL(c), channelNum(c), c.Name, c.ChannelID)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

type xspfPlaylist struct {
	XMLName   xml.Name      `xml:"playlist"`
	Version   string        `xml:"version,attr"`
	XMLNS     string        `xml:"xmlns,attr"`
	TrackList xspfTrackList `xml:"trackList"`
}
type xspfTrackList struct {
	Tracks []xspfTrack `xml:"track"`
}
type xspfTrack struct {
	Location   string `xml:"location"`
	Title      string `xml:"title"`
	Identifier string `xml:"identifier"`
	Image      string `xml:"image,omitempty"`
}

func (s *SiriusXM) ChannelsToXSPF() string {
	p := xspfPlaylist{Version: "1", XMLNS: "http://xspf.org/ns/0/"}
	for _, c := range sortedChannels(s.GetChannels()) {
		p.TrackList.Tracks = append(p.TrackList.Tracks, xspfTrack{Location: "/" + c.ChannelID + ".m3u8", Title: channelNum(c) + " " + c.Name, Identifier: c.ChannelID, Image: imageURL(c)})
	}
	b, _ := xml.MarshalIndent(p, "", "  ")
	return "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" + string(b)
}

func makeTextFrame(id, text string) []byte {
	enc := utf16BE(text)
	size := len(enc) + 1
	buf := bytes.NewBuffer(nil)
	buf.WriteString(id)
	binary.Write(buf, binary.BigEndian, uint32(size))
	buf.Write([]byte{0, 0, 1})
	buf.Write(enc)
	return buf.Bytes()
}
func utf16BE(s string) []byte {
	r := []rune(s)
	out := make([]byte, 0, len(r)*2+2)
	out = append(out, 0xFE, 0xFF)
	for _, v := range r {
		if v > 0xFFFF {
			v -= 0x10000
			hi := rune(0xD800 + (v >> 10))
			lo := rune(0xDC00 + (v & 0x3FF))
			binary.Write(bytes.NewBuffer(nil), binary.BigEndian, uint16(hi))
			out = append(out, byte(hi>>8), byte(hi), byte(lo>>8), byte(lo))
		} else {
			out = append(out, byte(v>>8), byte(v))
		}
	}
	return out
}
func syncsafe(i int) []byte {
	return []byte{byte(i >> 21 & 0x7f), byte(i >> 14 & 0x7f), byte(i >> 7 & 0x7f), byte(i & 0x7f)}
}

func DecryptAndInjectID3(data, aesKey []byte, artist, title, channelName, channelID string) ([]byte, error) {
	if len(data) < 16 {
		return nil, errors.New("encrypted segment too short")
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, err
	}
	if len(data[16:])%block.BlockSize() != 0 {
		return nil, errors.New("ciphertext is not block aligned")
	}
	plain := make([]byte, len(data)-16)
	cipher.NewCBCDecrypter(block, data[:16]).CryptBlocks(plain, data[16:])
	if n := int(plain[len(plain)-1]); n > 0 && n <= 16 {
		plain = plain[:len(plain)-n]
	}
	if len(plain) >= 10 && bytes.Equal(plain[:3], []byte("ID3")) {
		size := int(plain[6])<<21 | int(plain[7])<<14 | int(plain[8])<<7 | int(plain[9])
		if size+10 <= len(plain) {
			plain = plain[size+10:]
		}
	}
	frames := append(makeTextFrame("TIT2", channelID+" "+channelName+" | "+title), makeTextFrame("TPE1", artist)...)
	tag := append([]byte("ID3"), 0x03, 0x00, 0x00)
	tag = append(tag, syncsafe(len(frames))...)
	tag = append(tag, frames...)
	return append(tag, plain...), nil
}

var hlsAESKey = []byte(nil)

func init() { hlsAESKey, _ = base64.StdEncoding.DecodeString("0Nsco7MAgxowGvkUT8aYag==") }

func (s *SiriusXM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if x := recover(); x != nil {
			s.logf("request error: %v", x)
		}
	}()
	p := r.URL.Path
	if r.Method == http.MethodPost && p == "/server-mode" {
		var payload struct {
			ServerMode *bool `json:"server_mode"`
		}
		if json.NewDecoder(r.Body).Decode(&payload) != nil || payload.ServerMode == nil {
			http.Error(w, "Invalid JSON or missing 'server_mode' field", 400)
			return
		}
		json.NewEncoder(w).Encode(map[string]bool{"server_mode": s.SetServerMode(*payload.ServerMode)})
		return
	}
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	if p == "/server-mode" {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		json.NewEncoder(w).Encode(map[string]bool{"server_mode": s.GetServerMode()})
		return
	}
	if p == "/" || p == "/index.html" {
		serveLocalFile(w, "index.html", "text/html")
		return
	}
	if strings.HasPrefix(p, "/proxy/") {
		s.proxyImage(w, r)
		return
	}
	if strings.HasSuffix(p, ".m3u") {
		w.Header().Set("Content-Type", "audio/mpegurl")
		io.WriteString(w, s.ChannelsToM3U())
		return
	}
	if strings.HasSuffix(p, ".xspf") {
		w.Header().Set("Content-Type", "application/xspf+xml")
		io.WriteString(w, s.ChannelsToXSPF())
		return
	}
	if strings.HasSuffix(p, ".json") {
		name := strings.TrimSuffix(path.Base(p), ".json")
		_, cid, _, _, _ := s.GetChannel(name)
		s.mu.RLock()
		np, ok := s.nowPlaying[cid]
		s.mu.RUnlock()
		w.Header().Set("Content-Type", "text/json")
		if ok {
			json.NewEncoder(w).Encode(np)
		} else {
			io.WriteString(w, "[]")
		}
		return
	}
	if strings.HasSuffix(p, ".png") {
		serveLocalFile(w, "play.png", "image/png")
		return
	}
	if strings.HasSuffix(p, ".m3u8") {
		name := strings.TrimSuffix(path.Base(p), ".m3u8")
		data := s.GetPlaylist(name, true)
		if data == "" {
			http.Error(w, "playlist unavailable", 500)
			return
		}
		w.Header().Set("Content-Type", "application/x-mpegURL")
		io.WriteString(w, stripKeyLines(data))
		return
	}
	if strings.HasSuffix(p, ".cng") {
		ch := strings.TrimSuffix(path.Base(p), ".cng")
		s.mu.Lock()
		s.forceChannel = ch
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-mpegURL")
		return
	}
	if strings.HasSuffix(p, ".chn") {
		s.mu.RLock()
		ch := s.forceChannel
		s.mu.RUnlock()
		w.Header().Set("Content-Type", "text/plain;")
		io.WriteString(w, ch)
		return
	}
	if strings.HasSuffix(p, ".aac") {
		s.serveSegment(w, p)
		return
	}
	http.Error(w, "Internal Server Error", 500)
}
func stripKeyLines(s string) string {
	ls := strings.Split(s, "\n")
	out := ls[:0]
	for _, l := range ls {
		if !strings.HasPrefix(l, "#EXT-X-KEY") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
func serveLocalFile(w http.ResponseWriter, name, ct string) {
	b, err := os.ReadFile(name)
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Write(b)
}
func (s *SiriusXM) proxyImage(w http.ResponseWriter, r *http.Request) {
	raw, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/proxy/"))
	if err != nil {
		http.Error(w, "bad URL", 400)
		return
	}
	u, err := url.Parse(raw)
	if err != nil || !allowedHost(u.Hostname()) {
		http.Error(w, "Domain not allowed", 403)
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	res, err := s.client.Do(req)
	if err != nil {
		http.Error(w, "Proxy request failed", 500)
		return
	}
	defer res.Body.Close()
	w.Header().Set("Content-Type", res.Header.Get("Content-Type"))
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "image/png")
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(res.StatusCode)
	io.Copy(w, res.Body)
}
func allowedHost(h string) bool {
	switch strings.ToLower(h) {
	case "albumart.siriusxm.com", "pri.art.prod.streaming.siriusxm.com", "art.siriusxm.com":
		return true
	}
	return false
}
func (s *SiriusXM) serveSegment(w http.ResponseWriter, p string) {
	localPath := "/" + strings.TrimPrefix(p, "/")

	data := s.GetSegment(strings.TrimPrefix(p, "/"), 5)
	if data == nil {
		http.Error(w, "segment unavailable", http.StatusBadGateway)
		return
	}

	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if len(parts) < 2 {
		http.Error(w, "bad segment path", http.StatusBadRequest)
		return
	}

	cid := parts[0]

	_, channelCID, cname, _, cnum := s.GetChannel(cid)

	if channelCID == "" {
		channelCID = cid
	}

	s.mu.RLock()
	np := s.nowPlaying[channelCID]
	s.mu.RUnlock()

	artist := ""
	title := ""

	if len(np.Artists) > 0 {
		artist = np.Artists[0].Name
	}

	title = np.Title

	out, err := DecryptAndInjectID3(
		data,
		hlsAESKey,
		artist,
		title,
		cname,
		cnum,
	)

	if err != nil {
		s.logf("Segment processing failed for %s: %v", localPath, err)
		http.Error(w, "segment processing failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "audio/aac")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	_, _ = w.Write(out)
}

func RunServer(username, password string, port int) error {
	s := New(username, password)
	s.SetServerMode(true)
	return http.ListenAndServe(":"+strconv.Itoa(port), s)
}

// Keep html imported above available to callers that compare the original's
// HTML escaping behavior when generating custom pages.
var _ = html.EscapeString
var _ cipher.Block
