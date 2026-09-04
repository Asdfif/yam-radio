package main

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	apiBase        = "https://api.music.yandex.net"
	oauthBase      = "https://oauth.yandex.ru"
	defaultStation = "user:onyourwave"

	helpText = "Управление:\n" +
		"  1 — пауза / продолжить\n" +
		"  2 — следующий трек\n" +
		"  3 — добавить трек в избранное\n" +
		"  q / Ctrl+C — выход"
)

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Публичные значения официального Android-приложения Яндекс.Музыки:
// OAuth-креды (те же, что в open-source библиотеке yandex-music) и соль
// подписи прямых ссылок на MP3. Это не личные секреты, но при желании
// можно переопределить переменными окружения.
var (
	oauthClientID  = envOrDefault("YAM_OAUTH_CLIENT_ID", "23cabbbdc6cd418abb4b39c32c41195d")
	oauthClientSec = envOrDefault("YAM_OAUTH_CLIENT_SECRET", "53bc75238f0c4d08a118e51fe9203300")
	signSalt       = envOrDefault("YAM_SIGN_SALT", "XGRlBW9FXlekgbPrRHuSiA")
)

type flexID string

func (f *flexID) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexID(s)
		return nil
	}
	*f = flexID(bytes.TrimSpace(b))
	return nil
}

type artist struct {
	Name string `json:"name"`
}

type album struct {
	ID flexID `json:"id"`
}

type track struct {
	ID         flexID   `json:"id"`
	Title      string   `json:"title"`
	DurationMs int64    `json:"durationMs"`
	Artists    []artist `json:"artists"`
	Albums     []album  `json:"albums"`
}

func (t *track) trackID() string {
	if len(t.Albums) > 0 && t.Albums[0].ID != "" {
		return string(t.ID) + ":" + string(t.Albums[0].ID)
	}
	return string(t.ID)
}

func (t *track) artistsName() string {
	names := []string{}
	for _, a := range t.Artists {
		if a.Name != "" {
			names = append(names, a.Name)
		}
	}
	if len(names) == 0 {
		return "?"
	}
	return strings.Join(names, " / ")
}

type sequence struct {
	Type  string `json:"type"`
	Track *track `json:"track"`
}

type batch struct {
	BatchID  string     `json:"batchId"`
	Sequence []sequence `json:"sequence"`
}

type tokenData struct {
	AccessToken  string  `json:"access_token"`
	RefreshToken string  `json:"refresh_token"`
	ExpiresAt    float64 `json:"expires_at"`
}

func tokenPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "yam-radio", "token.json")
}

func loadToken() *tokenData {
	b, err := os.ReadFile(tokenPath())
	if err != nil {
		return nil
	}
	var td tokenData
	if json.Unmarshal(b, &td) != nil || td.AccessToken == "" {
		return nil
	}
	return &td
}

func saveToken(td *tokenData) {
	_ = os.MkdirAll(filepath.Dir(tokenPath()), 0o755)
	b, _ := json.MarshalIndent(td, "", "  ")
	_ = os.WriteFile(tokenPath(), b, 0o600)
}

func postOAuth(form url.Values) ([]byte, int) {
	resp, err := http.PostForm(oauthBase+"/token", form)
	if err != nil {
		return []byte(err.Error()), 0
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body, resp.StatusCode
}

func refreshToken(td *tokenData) error {
	body, status := postOAuth(url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {oauthClientID},
		"client_secret": {oauthClientSec},
		"refresh_token": {td.RefreshToken},
	})
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if json.Unmarshal(body, &out) != nil || out.AccessToken == "" {
		return fmt.Errorf("status=%d body=%s", status, strings.TrimSpace(string(body)))
	}
	td.AccessToken = out.AccessToken
	if out.RefreshToken != "" {
		td.RefreshToken = out.RefreshToken
	}
	td.ExpiresAt = float64(time.Now().Unix()) + float64(out.ExpiresIn)
	saveToken(td)
	return nil
}

func randDeviceID() string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 10)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

func deviceAuth() (*tokenData, error) {
	resp, err := http.PostForm(oauthBase+"/device/code", url.Values{
		"client_id":   {oauthClientID},
		"device_id":   {randDeviceID()},
		"device_name": {"YandexMusicAPI"},
	})
	if err != nil {
		return nil, err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("device/code: %d %s", resp.StatusCode, body)
	}
	var code struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURL string `json:"verification_url"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	if err := json.Unmarshal(body, &code); err != nil {
		return nil, err
	}
	fmt.Println("\n=== Авторизация Яндекс Музыки ===")
	fmt.Printf("Откройте ссылку: %s\n", code.VerificationURL)
	fmt.Printf("Введите код: %s\n", code.UserCode)
	fmt.Println("Жду подтверждения...")

	interval := code.Interval
	if interval <= 0 {
		interval = 5
	}
	if code.ExpiresIn <= 0 {
		code.ExpiresIn = 300
	}
	deadline := time.Now().Add(time.Duration(code.ExpiresIn) * time.Second)
	for {
		body, _ := postOAuth(url.Values{
			"grant_type":    {"device_code"},
			"code":          {code.DeviceCode},
			"client_id":     {oauthClientID},
			"client_secret": {oauthClientSec},
		})
		var out struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresIn    int    `json:"expires_in"`
			Error        string `json:"error"`
		}
		_ = json.Unmarshal(body, &out)
		if out.AccessToken != "" {
			td := &tokenData{
				AccessToken:  out.AccessToken,
				RefreshToken: out.RefreshToken,
				ExpiresAt:    float64(time.Now().Unix()) + float64(out.ExpiresIn),
			}
			saveToken(td)
			return td, nil
		}
		if out.Error == "slow_down" {
			interval += 5
		}
		if out.Error != "" && out.Error != "authorization_pending" && out.Error != "slow_down" {
			return nil, fmt.Errorf("oauth: %s", strings.TrimSpace(string(body)))
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("таймаут ожидания подтверждения")
		}
		time.Sleep(time.Duration(interval) * time.Second)
	}
}

type client struct {
	hc    *http.Client
	token string
	uid   int64
}

func newClient() (*client, error) {
	c := &client{hc: &http.Client{Timeout: 30 * time.Second}}
	td := loadToken()
	if td != nil && float64(time.Now().Unix()) < td.ExpiresAt-120 {
		c.token = td.AccessToken
		return c, nil
	}
	if td != nil && td.RefreshToken != "" {
		if err := refreshToken(td); err != nil {
			fmt.Printf("Не удалось обновить токен (%v), запускаю авторизацию заново...\n", err)
		} else {
			c.token = td.AccessToken
			return c, nil
		}
	}
	td2, err := deviceAuth()
	if err != nil {
		return nil, err
	}
	c.token = td2.AccessToken
	return c, nil
}

func (c *client) do(method, path string, query url.Values, form url.Values) (json.RawMessage, error) {
	ref := apiBase + path
	if len(query) > 0 {
		ref += "?" + query.Encode()
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, ref, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Yandex-Music-Client", "YandexMusicAndroid/24023621")
	req.Header.Set("User-Agent", "Yandex-Music-API")
	if c.token != "" {
		req.Header.Set("Authorization", "OAuth "+c.token)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("API %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return raw, nil
	}
	return env.Result, nil
}

func (c *client) init() error {
	raw, err := c.do("GET", "/account/status", nil, nil)
	if err != nil {
		return err
	}
	var st struct {
		Account struct {
			UID int64 `json:"uid"`
		} `json:"account"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return err
	}
	c.uid = st.Account.UID
	return nil
}

func (c *client) stationTracks(station, queue string) (*batch, error) {
	query := url.Values{}
	if queue == "" {
		query.Set("settings2", "True")
	} else {
		query.Set("queue", queue)
	}
	raw, err := c.do("GET", "/rotor/station/"+station+"/tracks", query, nil)
	if err != nil {
		return nil, err
	}
	var b batch
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

func (c *client) feedback(station, fbType, batchID, trackID, from string, played float64) {
	query := url.Values{}
	if batchID != "" {
		query.Set("batch-id", batchID)
	}
	form := url.Values{}
	form.Set("type", fbType)
	form.Set("timestamp", strconv.FormatFloat(float64(time.Now().UnixNano())/1e9, 'f', 3, 64))
	if trackID != "" {
		form.Set("trackId", trackID)
	}
	if from != "" {
		form.Set("from", from)
	}
	if played > 0 {
		form.Set("totalPlayedSeconds", strconv.FormatFloat(played, 'f', 0, 64))
	}
	if _, err := c.do("POST", "/rotor/station/"+station+"/feedback", query, form); err != nil {
		fmt.Printf("  [warn] фидбек не отправлен (%s): %v\n", fbType, err)
	}
}

type stationItem struct {
	Station struct {
		ID struct {
			Type string `json:"type"`
			Tag  string `json:"tag"`
		} `json:"id"`
		IDForFrom string `json:"idForFrom"`
	} `json:"station"`
	RupTitle   string `json:"rupTitle"`
	CustomName string `json:"customName"`
}

func (c *client) stationsList() ([]stationItem, error) {
	raw, err := c.do("GET", "/rotor/stations/list", url.Values{"language": {"ru"}}, nil)
	if err != nil {
		return nil, err
	}
	var items []stationItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	return items, nil
}

type downloadInfo struct {
	Codec           string `json:"codec"`
	BitrateInKbps   int    `json:"bitrateInKbps"`
	Preview         bool   `json:"preview"`
	DownloadInfoURL string `json:"downloadInfoUrl"`
}

type downloadInfoXML struct {
	Host string `xml:"host"`
	Path string `xml:"path"`
	Ts   string `xml:"ts"`
	S    string `xml:"s"`
}

func (c *client) streamURL(trackID string) (string, error) {
	raw, err := c.do("GET", "/tracks/"+trackID+"/download-info", nil, nil)
	if err != nil {
		return "", err
	}
	var infos []downloadInfo
	if err := json.Unmarshal(raw, &infos); err != nil {
		return "", err
	}
	bestIdx := -1
	for i := range infos {
		if infos[i].Preview {
			continue
		}
		if bestIdx == -1 || infos[i].BitrateInKbps > infos[bestIdx].BitrateInKbps {
			bestIdx = i
		}
	}
	if bestIdx == -1 && len(infos) > 0 {
		bestIdx = 0
	}
	if bestIdx == -1 {
		return "", fmt.Errorf("нет вариантов загрузки трека")
	}
	req, err := http.NewRequest("GET", infos[bestIdx].DownloadInfoURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Yandex-Music-Client", "YandexMusicAndroid/24023621")
	req.Header.Set("User-Agent", "Yandex-Music-API")
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var x downloadInfoXML
	if err := xml.NewDecoder(resp.Body).Decode(&x); err != nil {
		return "", err
	}
	trimmed := strings.TrimPrefix(x.Path, "/")
	sum := md5.Sum([]byte(signSalt + trimmed + x.S))
	return fmt.Sprintf("https://%s/get-mp3/%s/%s%s", x.Host, hex.EncodeToString(sum[:]), x.Ts, x.Path), nil
}

func (c *client) likeTrack(trackID string) error {
	if c.uid == 0 {
		return fmt.Errorf("нет uid аккаунта")
	}
	_, err := c.do("POST", fmt.Sprintf("/users/%d/likes/tracks/add-multiple", c.uid), nil, url.Values{"track-ids": {trackID}})
	return err
}

// Окно дедупа: сколько последних прозвучавших треков помним, чтобы
// не ставить их повторно. YAM_DEDUP_WINDOW, дефолт 100, мин. 1.
func dedupWindow() int {
	if v, err := strconv.Atoi(os.Getenv("YAM_DEDUP_WINDOW")); err == nil && v > 0 {
		return v
	}
	return 100
}

// Сколько раз подряд можно запрашивать новую пачку, если вся пачка состоит
// из уже прозвучавших треков, прежде чем играть первый доступный.
const maxBatchRefetches = 3

type wave struct {
	c         *client
	station   string
	from      string
	batch     *batch
	index     int
	cur       *track
	window    int
	recentSeq []string
	recentSet map[string]bool
}

func newWave(c *client, station string) *wave {
	w := &wave{
		c:         c,
		station:   station,
		window:    dedupWindow(),
		recentSet: map[string]bool{},
	}
	w.from = w.resolveFrom()
	return w
}

func (w *wave) resolveFrom() string {
	items, err := w.c.stationsList()
	if err == nil {
		for i := range items {
			s := &items[i]
			if s.Station.ID.Type+":"+s.Station.ID.Tag == w.station {
				if s.Station.IDForFrom != "" {
					return s.Station.IDForFrom
				}
				return s.Station.ID.Type
			}
		}
	}
	return strings.SplitN(w.station, ":", 2)[0]
}

func (w *wave) markRecent(id string) {
	if w.recentSet[id] {
		return
	}
	w.recentSet[id] = true
	w.recentSeq = append(w.recentSeq, id)
	if len(w.recentSeq) > w.window {
		old := w.recentSeq[0]
		w.recentSeq = w.recentSeq[1:]
		delete(w.recentSet, old)
	}
}

func (w *wave) refetchBatch(seed string) error {
	b, err := w.c.stationTracks(w.station, seed)
	if err != nil {
		return err
	}
	if len(b.Sequence) == 0 {
		return fmt.Errorf("пустая последовательность для станции %q", w.station)
	}
	w.batch = b
	w.c.feedback(w.station, "radioStarted", w.batch.BatchID, "", w.from, 0)
	w.index = 0
	return nil
}

func (w *wave) start() (*track, error) {
	if err := w.refetchBatch(""); err != nil {
		return nil, err
	}
	return w.take()
}

func (w *wave) next(playedFull bool, played float64) (*track, error) {
	if w.cur != nil {
		if playedFull {
			w.c.feedback(w.station, "trackFinished", w.batch.BatchID, w.cur.trackID(), "", played)
		} else {
			w.c.feedback(w.station, "skip", w.batch.BatchID, w.cur.trackID(), "", played)
		}
	}
	return w.take()
}

// take двигает курсор вперёд по пачке, тихо пропуская пустые элементы и
// треки, уже прозвучавшие в пределах окна дедупа, и добирает новую пачку,
// когда текущая кончается. Возвращает первый доступный трек.
func (w *wave) take() (*track, error) {
	refetches := 0
	for {
		if w.index >= len(w.batch.Sequence) {
			if refetches >= maxBatchRefetches {
				return w.takeAnyway()
			}
			seed := ""
			if w.cur != nil {
				seed = w.cur.trackID()
			}
			if err := w.refetchBatch(seed); err != nil {
				return nil, err
			}
			refetches++
			continue
		}
		seq := w.batch.Sequence[w.index]
		w.index++
		if seq.Track == nil {
			continue
		}
		if w.recentSet[seq.Track.trackID()] {
			continue
		}
		w.cur = seq.Track
		w.markRecent(w.cur.trackID())
		w.c.feedback(w.station, "trackStarted", w.batch.BatchID, w.cur.trackID(), "", 0)
		return w.cur, nil
	}
}

// takeAnyway играет первый реальный трек текущей пачки, даже если он уже
// прозвучал: радио не должно останавливаться из-за повтора.
func (w *wave) takeAnyway() (*track, error) {
	for i := range w.batch.Sequence {
		if w.batch.Sequence[i].Track != nil {
			w.index = i + 1
			w.cur = w.batch.Sequence[i].Track
			w.markRecent(w.cur.trackID())
			fmt.Printf("  [warn] доступные треки уже прозвучали, играю: %s\n", w.cur.Title)
			w.c.feedback(w.station, "trackStarted", w.batch.BatchID, w.cur.trackID(), "", 0)
			return w.cur, nil
		}
	}
	return nil, fmt.Errorf("нет доступных треков в пачке")
}

func mpvCmd(sock string, cmd []any) {
	conn, err := net.DialTimeout("unix", sock, 500*time.Millisecond)
	if err != nil {
		return
	}
	defer conn.Close()
	payload, _ := json.Marshal(map[string]any{"command": cmd})
	payload = append(payload, '\n')
	_ = conn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	_, _ = conn.Write(payload)
}

type stopFlag struct {
	once sync.Once
	ch   chan struct{}
}

func (s *stopFlag) stop() { s.once.Do(func() { close(s.ch) }) }

type termState struct {
	t syscall.Termios
}

func ioctlTermios(fd int, op uint, t *syscall.Termios) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(op), uintptr(unsafe.Pointer(t)))
	if errno != 0 {
		return errno
	}
	return nil
}

func setCBreak() *termState {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return nil
	}
	var t syscall.Termios
	if err := ioctlTermios(syscall.Stdin, syscall.TCGETS, &t); err != nil {
		return nil
	}
	t.Lflag &^= syscall.ICANON
	if err := ioctlTermios(syscall.Stdin, syscall.TCSETS, &t); err != nil {
		return nil
	}
	return &termState{t: t}
}

func (ts *termState) restore() {
	if ts != nil {
		_ = ioctlTermios(syscall.Stdin, syscall.TCSETS, &ts.t)
	}
}

func playOne(stream string, tr *track, c *client, sf *stopFlag, keyCh <-chan byte) (string, float64) {
	t0 := time.Now()
	dir, err := os.MkdirTemp("", "yam-")
	if err != nil {
		return "skipped", 0
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "mpv.sock")

	cmd := exec.Command("mpv", "--no-video", "--really-quiet", "--input-ipc-server="+sock, stream)
	cmd.Stdin = nil // иначе mpv перехватывает клавиши терминала (input-terminal)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return "skipped", 0
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	for i := 0; i < 100; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		select {
		case <-done:
			return "skipped", time.Since(t0).Seconds()
		case <-time.After(50 * time.Millisecond):
		}
	}

	skipped := false
	paused := false
	liked := false
	for {
		select {
		case waitErr := <-done:
			if skipped || waitErr != nil {
				return "skipped", time.Since(t0).Seconds()
			}
			return "finished", time.Since(t0).Seconds()
		case <-sf.ch:
			return "skipped", time.Since(t0).Seconds()
		case k := <-keyCh:
			switch k {
			case '1':
				paused = !paused
				mpvCmd(sock, []any{"set_property", "pause", paused})
				if paused {
					fmt.Println("[пауза]")
				} else {
					fmt.Println("[продолжить]")
				}
			case '2':
				skipped = true
				mpvCmd(sock, []any{"quit"})
			case '3':
				if !liked {
					liked = true
					if err := c.likeTrack(tr.trackID()); err != nil {
						fmt.Printf("[избранное] ошибка: %v\n", err)
					} else {
						fmt.Printf("[избранное] %s\n", tr.Title)
					}
				}
			case 'q', 0x03:
				mpvCmd(sock, []any{"quit"})
				sf.stop()
				return "skipped", time.Since(t0).Seconds()
			}
		}
	}
}

func run() error {
	station := defaultStation
	listOnly, checkOnly := false, false
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--list-stations":
			listOnly = true
		case "--check":
			checkOnly = true
		case "--station":
			if i+1 < len(args) {
				i++
				station = args[i]
			}
		}
	}

	if !listOnly {
		if _, err := exec.LookPath("mpv"); err != nil {
			return fmt.Errorf("mpv не найден. Установите: sudo apt install mpv")
		}
	}

	c, err := newClient()
	if err != nil {
		return fmt.Errorf("авторизация: %w", err)
	}
	if err := c.init(); err != nil {
		return fmt.Errorf("инициализация клиента: %w", err)
	}

	if listOnly {
		items, err := c.stationsList()
		if err != nil {
			return err
		}
		for i := range items {
			s := &items[i]
			name := s.CustomName
			if name == "" {
				name = s.RupTitle
			}
			fmt.Printf("%s:%s\t%s\n", s.Station.ID.Type, s.Station.ID.Tag, name)
		}
		return nil
	}

	if checkOnly {
		b, err := c.stationTracks(station, "")
		if err != nil || len(b.Sequence) == 0 || b.Sequence[0].Track == nil {
			return fmt.Errorf("получение треков станции: %w", err)
		}
		tr := b.Sequence[0].Track
		fmt.Printf("трек: %s — %s (%s)\n", tr.artistsName(), tr.Title, tr.trackID())
		if _, err := c.streamURL(tr.trackID()); err != nil {
			return fmt.Errorf("ссылка на стрим: %w", err)
		}
		fmt.Println("stream url: OK")
		return nil
	}

	fmt.Printf("\n«Моя волна»: станция %q\n\n", station)
	fmt.Println(helpText)

	w := newWave(c, station)
	cur, err := w.start()
	if err != nil {
		return fmt.Errorf("не удалось запустить станцию %q: %w (список станций: yam --list-stations)", station, err)
	}

	sf := &stopFlag{ch: make(chan struct{})}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		sf.stop()
		fmt.Println("\nОстанавливаюсь.")
	}()

	// Один reader на всё приложение: если создавать его в каждом playOne,
	// старые goroutine продолжают читать stdin и «съедают» клавиши.
	term := setCBreak()
	defer term.restore()
	keyCh := make(chan byte, 16)
	if term != nil {
		go func() {
			buf := make([]byte, 1)
			for {
				n, err := os.Stdin.Read(buf)
				if n > 0 {
					keyCh <- buf[0]
				}
				if err != nil {
					return
				}
			}
		}()
	}

	number := 0
	for {
		number++
		fmt.Printf("\n[%d] %s — %s\n", number, cur.artistsName(), cur.Title)
		var outcome string
		var played float64
		u, err := c.streamURL(cur.trackID())
		if err != nil {
			outcome = "skipped"
			fmt.Println("нет доступной ссылки на стрим, пропускаю")
		} else {
			outcome, played = playOne(u, cur, c, sf, keyCh)
		}
		select {
		case <-sf.ch:
			return nil
		default:
		}
		var err2 error
		cur, err2 = w.next(outcome == "finished", played)
		if err2 != nil {
			fmt.Println("ошибка получения следующего трека:", err2)
			return nil
		}
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Println("ошибка:", err)
		os.Exit(1)
	}
}
