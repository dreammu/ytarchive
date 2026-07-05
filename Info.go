package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dannav/hhmmss"
	"github.com/xhit/go-str2duration/v2"
)

const (
	DtypeAudio            = "audio"
	DtypeVideo            = "video"
	AudioItag             = 140
	AudioOnlyQuality      = 0
	BufferSize            = 8192
	DefaultFilenameFormat = "%(title)s-%(id)s"
	// 7 days in seconds
	LiveMaximumSeekable = 86400 * 7
)

const ytdlpInfoPrintTemplate = `{"id":%(id|null)j,"title":%(fulltitle,title|null)j,"channel_id":%(channel_id|null)j,"channel":%(channel|null)j,"description":%(description|null)j,"thumbnail":%(thumbnail|null)j,"upload_date":%(upload_date|null)j,"release_timestamp":%(release_timestamp|null)j,"timestamp":%(timestamp|null)j,"live_status":%(live_status|null)j,"is_live":%(is_live|null)j,"formats":%(formats.:.{format_id,url,protocol,manifest_url,target_duration})j}`

type VideoItag struct {
	H264 int
	VP9  int
	AV1  int
}

// https://gist.github.com/AgentOak/34d47c65b1d28829bb17c24c04a0096f
var (
	FilenameFormatBlacklist = []string{
		"description",
	}

	VideoLabelItags = map[string]VideoItag{
		"audio_only": {H264: 0, VP9: 0, AV1: 0},
		"144p":       {H264: 160, VP9: 278, AV1: 394},
		"240p":       {H264: 133, VP9: 242, AV1: 395},
		"360p":       {H264: 134, VP9: 243, AV1: 396},
		"480p":       {H264: 135, VP9: 244, AV1: 397},
		"720p":       {H264: 136, VP9: 247, AV1: 398},
		"720p60":     {H264: 298, VP9: 302, AV1: 398},
		"1080p":      {H264: 137, VP9: 248, AV1: 399},
		"1080p60":    {H264: 299, VP9: 303, AV1: 399},
		"1440p":      {H264: 264, VP9: 271, AV1: 400},
		"1440p60":    {H264: 304, VP9: 308, AV1: 400},
		"2160p":      {H264: 266, VP9: 313, AV1: 401},
		"2160p60":    {H264: 305, VP9: 315, AV1: 401},
	}

	VideoQualities = []string{
		"audio_only",
		"144p",
		"240p",
		"360p",
		"480p",
		"720p",
		"720p60",
		"1080p",
		"1080p60",
		"1440p",
		"1440p60",
		"2160p",
		"2160p60",
	}
)

/*
Simple class to more easily keep track of what fields are available for
file name formatting
*/
type FormatInfo map[string]string

/*
Metadata for the final file
*/
type MetaInfo map[string]string

/*
Info to be sent through the progress queue
*/
type ProgressInfo struct {
	Itag      int
	ByteCount int
	MaxSeq    int
	StartFrag int
}

/*
Fragment information/data
*/
type Fragment struct {
	Seq         int
	FileName    string
	XHeadSeqNum int
	Data        *bytes.Buffer
	Slow        bool
	MimeType    string
}

type seqChanInfo struct {
	CurSequence int
	MaxSequence int
}

/*
For sharing state between some functions used for downloading threads
*/
type fragThreadState struct {
	Name         string
	BaseFilePath string
	DataType     string
	SeqNum       int
	MaxSeq       int
	Tries        int
	FullRetries  int
	Is403        bool
	ToFile       bool
	SleepTime    time.Duration
}

type MediaDLInfo struct {
	sync.RWMutex
	ActiveJobs  int
	DownloadURL string
	BasePath    string
	DataType    string
	Finished    bool
	URLHost     string
}

/*
State for resumable downloading
*/
type DownloadState struct {
	StartFrag int
	Fragments int
	Size      int64
	TempDir   string
	File      string `json:"-"`
}

/*
Miscellaneous information
*/
type DownloadInfo struct {
	sync.RWMutex
	FormatInfo  FormatInfo
	Metadata    MetaInfo
	CookiesURL  *url.URL
	Ytcfg       *YTCFG
	VisitorData string
	PoToken     string

	Stopping         bool
	InProgress       bool
	Live             bool
	VP9              bool
	H264             bool
	AV1              bool
	YtdlpInfo        bool
	Unavailable      bool
	GVideoDDL        bool
	FragFiles        bool
	LiveURL          bool
	AudioOnly        bool
	VideoOnly        bool
	MembersOnly      bool
	InfoPrinted      bool
	DisableSaveState bool

	Thumbnail       string
	VideoID         string
	URL             string
	SelectedQuality string
	Status          string
	LiveFromVal     string
	YtdlpPath       string
	YtdlpOpts       string

	FragMaxTries        uint
	Wait                int
	Quality             int
	RetrySecs           int
	Jobs                int
	TargetDuration      int
	LastSq              int
	LiveFromSq          int
	CaptureDurationSecs int
	StartDelaySecs      int
	LastUpdated         time.Time

	MDLInfo map[string]*MediaDLInfo
	DLState map[int]*DownloadState

	FileMode os.FileMode
	DirMode  os.FileMode
}

func NewDownloadInfo() *DownloadInfo {
	return &DownloadInfo{
		FragFiles:      true,
		Wait:           ActionAsk,
		Quality:        -1,
		Jobs:           1,
		TargetDuration: 5,
		FormatInfo:     NewFormatInfo(),
		Metadata:       NewMetaInfo(),
		MDLInfo: map[string]*MediaDLInfo{
			DtypeVideo: {},
			DtypeAudio: {},
		},
		DLState: make(map[int]*DownloadState),
	}
}

func NewFragThreadState(name, baseFPath, dataType string, toFile bool, sleepTime time.Duration) *fragThreadState {
	return &fragThreadState{
		Name:         name,
		BaseFilePath: baseFPath,
		DataType:     dataType,
		ToFile:       toFile,
		SleepTime:    sleepTime,
	}
}

func NewFormatInfo() FormatInfo {
	return FormatInfo{
		"id":           "",
		"title":        "",
		"channel_id":   "",
		"channel":      "",
		"upload_date":  "",
		"start_date":   "",
		"year":         "",
		"month":        "",
		"day":          "",
		"start_time":   "",
		"hours":        "",
		"minutes":      "",
		"seconds":      "",
		"publish_date": "",
		"description":  "",
		"url":          "",
	}
}

func NewMetaInfo() MetaInfo {
	return MetaInfo{
		"title":   "%(title)s",
		"artist":  "%(channel)s",
		"date":    "%(upload_date)s",
		"comment": "%(url)s\n\n%(description)s",
	}
}

func (di *DownloadInfo) IsStopping() bool {
	di.RLock()
	defer di.RUnlock()
	return di.Stopping
}

func (di *DownloadInfo) Stop() {
	di.Lock()
	defer di.Unlock()
	di.Stopping = true
	di.SetFinished(DtypeAudio)
	di.SetFinished(DtypeVideo)
}

func (di *DownloadInfo) IsLive() bool {
	di.RLock()
	defer di.RUnlock()
	return di.Live
}

func (di *DownloadInfo) IsUnavailable() bool {
	di.RLock()
	defer di.RUnlock()
	return di.Unavailable
}

func (di *DownloadInfo) IsGVideoDDL() bool {
	di.RLock()
	defer di.RUnlock()
	return di.GVideoDDL
}

func (di *DownloadInfo) GetActiveJobCount(dataType string) int {
	di.MDLInfo[dataType].RLock()
	defer di.MDLInfo[dataType].RUnlock()
	return di.MDLInfo[dataType].ActiveJobs
}

func (di *DownloadInfo) IncrementJobs(dataType string) {
	di.MDLInfo[dataType].Lock()
	defer di.MDLInfo[dataType].Unlock()
	di.MDLInfo[dataType].ActiveJobs += 1
}

func (di *DownloadInfo) DecrementJobs(dataType string) {
	di.MDLInfo[dataType].Lock()
	defer di.MDLInfo[dataType].Unlock()
	di.MDLInfo[dataType].ActiveJobs -= 1
}

func (di *DownloadInfo) GetDownloadUrl(dataType string) string {
	di.MDLInfo[dataType].RLock()
	defer di.MDLInfo[dataType].RUnlock()
	return di.MDLInfo[dataType].DownloadURL
}

func (di *DownloadInfo) SetDownloadUrl(dataType, dlURL string) {
	di.MDLInfo[dataType].Lock()
	defer di.MDLInfo[dataType].Unlock()

	purl, err := url.Parse(dlURL)
	if err == nil {
		di.MDLInfo[dataType].URLHost = purl.Host
	}

	di.MDLInfo[dataType].DownloadURL = dlURL
}

func (di *DownloadInfo) GetDownloadUrlHost(dataType string) string {
	di.MDLInfo[dataType].RLock()
	defer di.MDLInfo[dataType].RUnlock()
	return di.MDLInfo[dataType].URLHost
}

func (di *DownloadInfo) GetBaseFilePath(dataType string) string {
	di.MDLInfo[dataType].RLock()
	defer di.MDLInfo[dataType].RUnlock()
	return di.MDLInfo[dataType].BasePath
}

func (di *DownloadInfo) SetBaseFilePath(dataType, fpath string) {
	di.MDLInfo[dataType].Lock()
	defer di.MDLInfo[dataType].Unlock()
	di.MDLInfo[dataType].BasePath = fpath
}

func (di *DownloadInfo) SetFinished(dataType string) {
	di.MDLInfo[dataType].Lock()
	defer di.MDLInfo[dataType].Unlock()
	di.MDLInfo[dataType].Finished = true
}

func (di *DownloadInfo) IsFinished(dataType string) bool {
	di.MDLInfo[dataType].RLock()
	defer di.MDLInfo[dataType].RUnlock()
	return di.MDLInfo[dataType].Finished
}

func (di *DownloadInfo) GetTimeSinceUpdated() time.Duration {
	di.RLock()
	defer di.RUnlock()
	return time.Since(di.LastUpdated)
}

func (fi FormatInfo) SetInfo(player_response *PlayerResponse) {
	pmfr := player_response.Microformat.PlayerMicroformatRenderer
	vid := player_response.VideoDetails.VideoID
	startDate := strings.ReplaceAll(pmfr.LiveBroadcastDetails.StartTimestamp, "-", "")
	year, month, day, hours, minutes, seconds := "", "", "", "", "", ""
	startTime := strings.ReplaceAll(pmfr.LiveBroadcastDetails.StartTimestamp, ":", "")
	publishDate := strings.ReplaceAll(pmfr.PublishDate, "-", "")
	url := fmt.Sprintf("https://www.youtube.com/watch?v=%s", vid)

	if len(startDate) > 0 {
		startDate = startDate[:8]
		year = startDate[:4]
		month = startDate[4:6]
		day = startDate[6:8]
	}

	if len(startTime) > 0 {
		startTime = startTime[11:17]
		hours = startTime[:2]
		minutes = startTime[2:4]
		seconds = startTime[4:6]
	}

	fi["id"] = vid
	fi["url"] = url
	fi["title"] = strings.TrimSpace(player_response.VideoDetails.Title)
	fi["channel_id"] = player_response.VideoDetails.ChannelID
	fi["channel"] = player_response.VideoDetails.Author
	fi["upload_date"] = startDate
	fi["start_date"] = startDate
	fi["year"] = year
	fi["month"] = month
	fi["day"] = day
	fi["start_time"] = startTime
	fi["hours"] = hours
	fi["minutes"] = minutes
	fi["seconds"] = seconds
	fi["publish_date"] = publishDate
	fi["description"] = strings.TrimSpace(player_response.VideoDetails.ShortDescription)
}

func (mi MetaInfo) SetInfo(fi FormatInfo) {
	for k, v := range mi {
		val, err := FormatPythonMapString(v, fi)
		if err != nil {
			// ignore and just leave unformatted
			continue
		}

		mi[k] = val
	}
}

func LogRetryStatus(retryCount int, totalWaited int) {
	if loglevel <= LoglevelQuiet {
		return
	}
	msg := "Retries: %d (Last retry: %s), Total time waited: %d seconds"
	if !statusNewlines {
		msg = "\r" + msg
	} else {
		msg += "\n"
	}
	fmt.Fprintf(os.Stderr, msg, retryCount, time.Now().Format("2006/01/02 15:04:05"), totalWaited)
}

func (di *DownloadInfo) printStatusWithoutLock() {
	if loglevel >= LoglevelError {
		fmt.Print(di.Status)
	}
}

func (di *DownloadInfo) SetStatus(status string) {
	di.Lock()
	defer di.Unlock()
	di.Status = status
	di.printStatusWithoutLock()
}

func (di *DownloadInfo) PrintStatus() {
	di.RLock()
	defer di.RUnlock()

	di.printStatusWithoutLock()
}

func (di *DownloadInfo) SaveState(itag int) {
	if di.DisableSaveState || len(di.DLState[itag].File) == 0 {
		return
	}

	data, err := json.Marshal(di.DLState[itag])
	if err != nil {
		LogWarn("Error when saving state: %s", err)
		return
	}

	err = os.WriteFile(di.DLState[itag].File, data, di.FileMode)
	if err != nil {
		LogWarn("Error when saving state: %s", err)
		return
	}
}

// Ask if the user wants to wait for a scheduled stream to start and then record it
func (di *DownloadInfo) AskWaitForStream() bool {
	LogGeneral("%s\n%s\n",
		"This stream is likely a future scheduled livestream.",
		"Would you like to wait for the scheduled start time, poll until it starts, or not wait?",
	)
	choice := strings.ToLower(GetUserInput("wait/poll/[no]: "))

	if strings.HasPrefix(choice, "wait") {
		di.Wait = ActionDo
		return true
	} else if strings.HasPrefix(choice, "poll") {
		secs := GetUserInput("Input poll interval in seconds (minimum 15): ")
		s, err := strconv.Atoi(secs)
		if err != nil || s < DefaultPollTime {
			s = DefaultPollTime
		}

		di.RetrySecs = s
		return true
	}

	return false
}

func (di *DownloadInfo) GetGvideoUrl(dataType string) {
	for {
		gvUrl := GetUserInput(fmt.Sprintf("Please enter the %s url, or nothing to skip: ", dataType))
		if len(gvUrl) == 0 {
			return
		}

		newUrl, itag := ParseGvideoUrl(gvUrl, dataType)
		if len(newUrl) == 0 {
			continue
		}

		if dataType == DtypeVideo {
			di.Quality = itag
		}

		if (dataType == DtypeAudio && itag == AudioItag) ||
			(dataType == DtypeVideo && itag != AudioItag) {
			di.SetDownloadUrl(dataType, newUrl)
			break
		} else {
			LogGeneral("URL given does not appear to be appropriate for the data type needed.")
		}
	}
}

// Execute yt-dlp command to get stream info
func (di *DownloadInfo) ExecuteYtdlp() ([]byte, error) {
	args := []string{"--live-from-start"}

	// Add cookies parameter
	if len(cookieFile) > 0 {
		args = append(args, "--cookies", cookieFile)
	}

	// Add proxy parameter
	if proxyUrl != nil {
		args = append(args, "--proxy", proxyUrl.String())
	}

	if di.YtdlpInfo {
		args = append(args, "--ignore-no-formats-error")
	}

	// Add custom yt-dlp options
	if len(di.YtdlpOpts) > 0 {
		// Split the options string by spaces, respecting quoted strings
		customArgs := strings.Fields(di.YtdlpOpts)
		args = append(args, customArgs...)
	}
	args = append(args, "--print", ytdlpInfoPrintTemplate)

	// Add URL
	args = append(args, di.URL)

	// Execute command with 30 second timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, di.YtdlpPath, args...)
	output, err := cmd.Output()

	return output, err
}

// Execute yt-dlp with retry logic
func (di *DownloadInfo) ExecuteYtdlpWithRetry(maxRetries int) []byte {
	for attempt := 1; attempt <= maxRetries; attempt++ {
		output, err := di.ExecuteYtdlp()
		if err == nil {
			return output
		}

		LogWarn("yt-dlp attempt %d/%d failed: %v", attempt, maxRetries, err)
		if attempt < maxRetries {
			time.Sleep(2 * time.Second)
		}
	}

	LogWarn("Failed to get stream info from yt-dlp after %d attempts", maxRetries)
	return nil
}

// Parse yt-dlp JSON output to extract adaptive format URLs
func parseItagFromFormatID(formatID string) int {
	left, _, _ := strings.Cut(formatID, "-")
	itag, _ := strconv.Atoi(left)
	return itag
}

type YtdlpFormat struct {
	FormatID       string  `json:"format_id"`
	URL            string  `json:"url"`
	Protocol       string  `json:"protocol"`
	ManifestURL    string  `json:"manifest_url"`
	TargetDuration float64 `json:"target_duration"`
}

type YtdlpExtraction struct {
	URLs           map[int]string
	AdaptiveURLs   map[int]string
	LastSq         int
	TargetDuration int
	ID             string
	Title          string
	ChannelID      string
	Channel        string
	Description    string
	Thumbnail      string
	UploadDate     string
	StartDate      string
	StartUnix      int64
	LiveStatus     string
	IsLive         bool
}

func (di *DownloadInfo) seekableStartSqFromLastSq(lastSq int) int {
	if lastSq < 0 || di.TargetDuration <= 0 {
		return 0
	}

	startSq := lastSq - (LiveMaximumSeekable / di.TargetDuration)
	if startSq < 0 {
		return 0
	}
	return startSq
}

func formatUnixDate(timestamp int64) (date, clock, year, month, day, hours, minutes, seconds string) {
	if timestamp <= 0 {
		return "", "", "", "", "", "", "", ""
	}

	t := time.Unix(timestamp, 0).UTC()
	date = t.Format("20060102")
	clock = t.Format("150405")
	return date, clock, t.Format("2006"), t.Format("01"), t.Format("02"), t.Format("15"), t.Format("04"), t.Format("05")
}

func formatLocalTimestamp(timestamp string) string {
	timestamp = strings.TrimSpace(timestamp)
	if timestamp == "" {
		return ""
	}

	t, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return timestamp
	}
	return t.Local().Format(time.RFC3339)
}

func formatLocalUnixTimestamp(timestamp int64) string {
	if timestamp <= 0 {
		return ""
	}

	return time.Unix(timestamp, 0).Local().Format(time.RFC3339)
}

func probeYtdlpAdaptiveSeqBoundary(baseURL string, postLive bool) int {
	headerSeqnum := probeXHeadSeqnum(baseURL)
	if headerSeqnum < 0 {
		return -1
	}

	if postLive {
		// Post-live base URL probes commonly have an empty body, but the
		// X-Head-Seqnum header is still available. Use one less than the
		// header as the exclusive stop boundary so the downloader avoids
		// the trailing fragments that tend to return 503.
		if headerSeqnum <= 1 {
			return -1
		}
		return headerSeqnum - 1
	}
	return headerSeqnum
}

func probeXHeadSeqnum(probeURL string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", probeURL, nil)
	if err != nil {
		return -1
	}
	req.Header.Add("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:152.0) Gecko/20100101 Firefox/152.0")
	req.Header.Add("Origin", "https://www.youtube.com")

	resp, err := client.Do(req)
	if err != nil {
		return -1
	}
	defer func() {
		if resp.Body != nil {
			resp.Body.Close()
		}
	}()

	headerSeqnumStr := resp.Header.Get("X-Head-Seqnum")
	if headerSeqnumStr == "" {
		return -1
	}
	lastSq, err := strconv.Atoi(headerSeqnumStr)
	if err != nil {
		return -1
	}
	return lastSq
}

func (fi FormatInfo) SetYtdlpInfo(info *YtdlpExtraction, pageURL string) {
	date := info.UploadDate
	startDate := info.StartDate
	year, month, day := "", "", ""
	hours, minutes, seconds := "", "", ""
	startTime := ""

	if info.StartUnix > 0 {
		startDate, startTime, year, month, day, hours, minutes, seconds = formatUnixDate(info.StartUnix)
	} else if len(startDate) >= 8 {
		year = startDate[:4]
		month = startDate[4:6]
		day = startDate[6:8]
	}

	fi["id"] = info.ID
	fi["url"] = pageURL
	fi["title"] = strings.TrimSpace(info.DisplayTitle())
	fi["channel_id"] = info.ChannelID
	fi["channel"] = info.Channel
	fi["upload_date"] = date
	fi["start_date"] = startDate
	fi["year"] = year
	fi["month"] = month
	fi["day"] = day
	fi["start_time"] = startTime
	fi["hours"] = hours
	fi["minutes"] = minutes
	fi["seconds"] = seconds
	fi["publish_date"] = date
	fi["description"] = strings.TrimSpace(info.Description)
}

func (info *YtdlpExtraction) DisplayTitle() string {
	return strings.TrimSpace(info.Title)
}

func (info *YtdlpExtraction) IsUpcoming() bool {
	return info.LiveStatus == "is_upcoming"
}

func (info *YtdlpExtraction) IsProcessedVOD() bool {
	return info.LiveStatus == "was_live"
}

func (info *YtdlpExtraction) IsNotLive() bool {
	return info.LiveStatus == "not_live"
}

func (info *YtdlpExtraction) IsPostLive() bool {
	return info.LiveStatus == "post_live"
}

func (di *DownloadInfo) FinishYtdlpLiveRefresh(message string) bool {
	di.Lock()
	defer di.Unlock()

	if !di.InProgress {
		LogGeneral("%s", message)
		return false
	}

	LogDebug("%s", message)
	di.Live = false
	return false
}

func (di *DownloadInfo) YtdlpWaitInterval(info *YtdlpExtraction) (time.Duration, bool) {
	if di.Wait == ActionDoNot {
		return 0, false
	}
	if di.RetrySecs > 0 {
		return time.Duration(di.RetrySecs) * time.Second, true
	}

	switch di.Wait {
	case ActionDo:
		if info != nil && info.StartUnix > 0 {
			if interval := time.Until(time.Unix(info.StartUnix, 0)); interval > 0 {
				return interval, true
			}
		}
		return DefaultPollTime * time.Second, true
	default:
		if !di.AskWaitForStream() {
			return 0, false
		}
		if di.RetrySecs > 0 {
			return time.Duration(di.RetrySecs) * time.Second, true
		}
		if info != nil && info.StartUnix > 0 {
			if interval := time.Until(time.Unix(info.StartUnix, 0)); interval > 0 {
				return interval, true
			}
		}
		return DefaultPollTime * time.Second, true
	}
}

func (di *DownloadInfo) applyYtdlpFormat(info *YtdlpExtraction, format YtdlpFormat, lastSqProbed *bool) {
	itag := parseItagFromFormatID(format.FormatID)
	if itag == 0 {
		return
	}

	if format.TargetDuration > 0 && info.TargetDuration <= 0 {
		info.TargetDuration = int(math.Round(format.TargetDuration))
	}

	if !isYtdlpAdaptiveFormat(format) {
		return
	}

	info.AdaptiveURLs[itag] = strings.ReplaceAll(format.URL, "%", "%%") + "&sq=%d"
	if info.LastSq < 0 && !*lastSqProbed {
		*lastSqProbed = true
		if sq := probeYtdlpAdaptiveSeqBoundary(format.URL, info.IsPostLive()); sq >= 0 {
			info.LastSq = sq
		}
	}
}

func isYtdlpAdaptiveFormat(format YtdlpFormat) bool {
	if format.URL == "" || !IsFragmented(format.URL) {
		return false
	}
	if format.ManifestURL != "" {
		return false
	}

	switch format.Protocol {
	case "http_dash_segments", "http_dash_segments_generator":
		return true
	default:
		return false
	}
}

func (di *DownloadInfo) ParseYtdlpJsonInfo(jsonData []byte) (*YtdlpExtraction, error) {
	info := &YtdlpExtraction{
		URLs:         make(map[int]string),
		AdaptiveURLs: make(map[int]string),
		LastSq:       -1,
	}

	var payload struct {
		ID               string        `json:"id"`
		Title            string        `json:"title"`
		ChannelID        string        `json:"channel_id"`
		Channel          string        `json:"channel"`
		Description      string        `json:"description"`
		Thumbnail        string        `json:"thumbnail"`
		UploadDate       string        `json:"upload_date"`
		ReleaseTimestamp int64         `json:"release_timestamp"`
		Timestamp        int64         `json:"timestamp"`
		LiveStatus       string        `json:"live_status"`
		IsLive           bool          `json:"is_live"`
		Formats          []YtdlpFormat `json:"formats"`
	}

	if err := json.Unmarshal(jsonData, &payload); err != nil {
		return nil, err
	}

	info.ID = payload.ID
	info.Title = payload.Title
	info.ChannelID = payload.ChannelID
	info.Channel = payload.Channel
	info.Description = payload.Description
	info.Thumbnail = payload.Thumbnail
	info.UploadDate = payload.UploadDate
	info.LiveStatus = payload.LiveStatus
	info.IsLive = payload.IsLive || payload.LiveStatus == "is_live"
	if payload.ReleaseTimestamp > 0 {
		info.StartUnix = payload.ReleaseTimestamp
		info.StartDate, _, _, _, _, _, _, _ = formatUnixDate(payload.ReleaseTimestamp)
	} else if payload.Timestamp > 0 {
		info.StartUnix = payload.Timestamp
		info.StartDate, _, _, _, _, _, _, _ = formatUnixDate(payload.Timestamp)
	}

	lastSqProbed := false
	for _, format := range payload.Formats {
		di.applyYtdlpFormat(info, format, &lastSqProbed)
	}

	for itag, dlURL := range info.AdaptiveURLs {
		info.URLs[itag] = dlURL
	}
	if info.TargetDuration > 0 {
		di.TargetDuration = info.TargetDuration
	}

	return info, nil
}

func (di *DownloadInfo) ParseYtdlpJson(jsonData []byte) (map[int]string, int) {
	info, err := di.ParseYtdlpJsonInfo(jsonData)
	if err != nil {
		LogDebug("Failed to parse yt-dlp json: %v", err)
		return map[int]string{}, -1
	}

	if len(info.AdaptiveURLs) > 0 {
		LogDebug("Loaded %d adaptive format URLs from yt-dlp", len(info.AdaptiveURLs))
	}

	return info.AdaptiveURLs, info.LastSq
}

func (di *DownloadInfo) ParseLiveFromStrVal() error {
	if di.LiveFromVal == "" {
		return nil
	}

	if strings.ToLower(di.LiveFromVal) == "now" {
		// --live-from now
		//  Seek to current sequence number
		di.LiveFromSq = di.LastSq
		LogGeneral("Starting download from current time")
	} else {
		durationVal := strings.TrimPrefix(di.LiveFromVal, "-") // Removes negative symbol from start of duration string

		// Try to parse the value as a duration string
		duration, err := str2duration.ParseDuration(durationVal)
		if err != nil {
			// Try to parse the value as a HH:MM:SS string
			duration, err = hhmmss.Parse(durationVal)
			if err != nil {
				LogError("Unable to parse value as either a duration or a time string: %v", err)
				return err
			}
		}

		secondsTotal := duration.Seconds()
		fragDur := float64(di.TargetDuration)
		secondsRoundedToFragLength := int(math.Ceil(secondsTotal/fragDur) * fragDur) // Rounds up to next frag interval time
		noOfFragsToJump := secondsRoundedToFragLength / di.TargetDuration

		if strings.HasPrefix(di.LiveFromVal, "-") {
			// --live-from negative value
			//  Seek to a sequence number in the past

			// Invalid time specification (too short or too long)
			if secondsTotal < 0 || secondsTotal > LiveMaximumSeekable {
				LogError("Invalid duration specified '%s'. (Maximum video seek time is %d days)", di.LiveFromVal, (LiveMaximumSeekable / 60 / 60 / 24))
				return errors.New("invalid duration specified")
			}
			// If the stream hasn't been live long enough for the specified duration
			if noOfFragsToJump > di.LastSq {
				streamLength := di.LastSq * di.TargetDuration
				curStreamDuration := SecondsToDurationAndTimeStr(streamLength)

				LogError("Invalid duration specified. The stream has not been live for that long [Live for %s].", curStreamDuration)
				return errors.New("invalid duration specified")
			}

			di.LiveFromSq = di.LastSq - noOfFragsToJump
			LogGeneral("Jumping back %d seconds from now, and starting to download from that time.", secondsRoundedToFragLength)
			LogDebug("Jumping back %d frags. Will start from sequence %d [current is %d].", noOfFragsToJump, di.LiveFromSq, di.LastSq)
		} else {
			// --live-from positive value
			// Calculate the sequence number of the specified stream time to start from.
			maxSq := di.LastSq
			targetStartFrag := noOfFragsToJump

			// Stream hasn't been live long enough
			if di.LastSq < targetStartFrag {
				streamLength := di.LastSq * di.TargetDuration
				curStreamDuration := SecondsToDurationAndTimeStr(streamLength)

				errStr := fmt.Errorf("invalid duration specified. the stream has not been live for that long [live for %s]", curStreamDuration)
				return errors.New(errStr.Error())
			} else {
				// Make sure the Start Frag is within the 7 day limit.
				if targetStartFrag < (di.LastSq - LiveMaximumSeekable) {
					LogError("YT only retains the livestream 7 days past for seeking, your --live-from value of '%s' is not valid.", di.LiveFromVal)

					// Calculate how long the stream has been live for
					streamLiveTime := di.LastSq * di.TargetDuration
					minSeekTime := streamLiveTime - LiveMaximumSeekable
					LogError("You must specify a --live-from value between: %s and %s", SecondsToDurationAndTimeStr(minSeekTime), SecondsToDurationAndTimeStr(streamLiveTime))
					return errors.New("value is not valid for stream duration")
				}

				di.LiveFromSq = targetStartFrag
				startTimeStr := SecondsToDurationAndTimeStr(di.LiveFromSq * di.TargetDuration)
				totalTimeToGrabStr := SecondsToDurationAndTimeStr((maxSq - di.LiveFromSq) * di.TargetDuration)
				LogGeneral("Starting from stream time '%s' and grabbing '%s' of content (and counting).", startTimeStr, totalTimeToGrabStr)
				LogDebug("Starting from sequence %d [max right now is %d]", di.LiveFromSq, maxSq)
			}
		}
	}

	return nil
}

func (di *DownloadInfo) ParseInputUrl() error {
	parsedUrl, err := url.Parse(di.URL)
	if err != nil {
		return err
	}

	lowerHost := strings.ToLower(parsedUrl.Host)
	lowerHost = strings.TrimPrefix(lowerHost, "www.")
	lowerHost = strings.TrimPrefix(lowerHost, "m.")
	lowerPath := strings.ToLower(parsedUrl.EscapedPath())
	parsedQuery := parsedUrl.Query()

	if lowerHost == "youtube.com" {
		if strings.HasPrefix(lowerPath, "/watch") {
			if _, ok := parsedQuery["v"]; !ok {
				return errors.New("youtube URL missing video ID")
			}

			di.VideoID = parsedQuery.Get("v")
			return nil
		} else if strings.HasPrefix(lowerPath, "/channel/") ||
			strings.HasPrefix(lowerPath, "/c/") ||
			strings.HasPrefix(lowerPath, "/user/") ||
			strings.HasPrefix(lowerPath, "/@") {
			// The URL can be polled and the stream can change depending on what
			// the channel schedules. Useful for set-and-forget
			chanSlashIdx := strings.Index(lowerPath[1:], "/") + 1
			noChanPath := lowerPath[chanSlashIdx:]

			// Check if we were given the channel url on a sub page
			// Remove that part from the URL so we can append /live to it after
			if strings.LastIndex(noChanPath, "/") > 0 {
				lastSlash := strings.LastIndex(di.URL, "/")
				di.URL = di.URL[:lastSlash]
			}

			di.URL = fmt.Sprintf("%s/live", di.URL)
			di.LiveURL = true
			return nil
		} else if strings.HasPrefix(lowerPath, "/live/") {
			di.VideoID = strings.TrimPrefix(parsedUrl.EscapedPath(), "/live/")
			return nil
		} else if strings.HasPrefix(lowerPath, "/shorts/") {
			di.VideoID = strings.TrimPrefix(parsedUrl.EscapedPath(), "/shorts/")
			return nil
		}
	} else if lowerHost == "youtu.be" {
		di.VideoID = strings.TrimLeft(parsedUrl.EscapedPath(), "/")
		return nil
	} else if strings.HasSuffix(lowerHost, ".googlevideo.com") {
		if _, ok := parsedQuery["noclen"]; !ok {
			return errors.New("given Google Video URL is not for a fragmented stream")
		}

		di.GVideoDDL = true
		id := parsedQuery.Get("id")
		dotIdx := strings.LastIndex(id, ".")
		id = id[:dotIdx]
		di.VideoID = id
		di.FormatInfo["id"] = di.VideoID
		sqIdx := strings.Index(di.URL, "&sq=")
		itag, err := strconv.Atoi(parsedQuery.Get("itag"))

		if err != nil {
			return fmt.Errorf("error parsing itag parameter of Google Video URL: %s", err)
		}

		if sqIdx < 0 {
			return errors.New("could not find 'sq' parameter in given Google Video URL")
		}

		if itag == AudioItag {
			if len(di.GetDownloadUrl(DtypeAudio)) == 0 {
				di.SetDownloadUrl(DtypeAudio, di.URL[:sqIdx]+"&sq=%d")
			}

			if len(di.GetDownloadUrl(DtypeVideo)) == 0 && !di.AudioOnly {
				di.GetGvideoUrl(DtypeVideo)
			}
		} else {
			if len(di.GetDownloadUrl(DtypeVideo)) == 0 {
				di.SetDownloadUrl(DtypeVideo, di.URL[:sqIdx]+"&sq=%d")
			}

			if len(di.GetDownloadUrl(DtypeAudio)) == 0 && !di.VideoOnly {
				di.GetGvideoUrl(DtypeAudio)
			}
		}

		di.Quality = itag
		return nil
	}

	return errors.New("The provided URL is not a known valid YouTube URL")
}

/*
Get download URLs from generated adaptive formats extracted by yt-dlp.
*/
func (di *DownloadInfo) GetDownloadUrls(pr *PlayerResponse) map[int]string {
	_ = pr
	urls := make(map[int]string)

	if di.YtdlpPath != "" {
		jsonData := di.ExecuteYtdlpWithRetry(3)
		if jsonData != nil {
			adaptiveUrls, ytdlpLastSq := di.ParseYtdlpJson(jsonData)
			if len(adaptiveUrls) > 0 {
				for itag, url := range adaptiveUrls {
					urls[itag] = url
					LogTrace("Setting itag %d from yt-dlp adaptive formats", itag)
				}
				if ytdlpLastSq > 0 {
					di.LastSq = ytdlpLastSq
				}

				return urls
			}
		}
	}

	return urls
}

func (di *DownloadInfo) ParseCaptureDurationStrVal(durationVal string) error {
	if durationVal == "" {
		return nil
	}

	// Try to parse the value as a duration string
	duration, err := str2duration.ParseDuration(durationVal)
	if err != nil {
		// Try to parse the value as a HH:MM:SS string
		duration, err = hhmmss.Parse(durationVal)
		if err != nil {
			LogError("Unable to parse value as either a Duration or a Time String: %v", err)
			return errors.New("invalid duration string")
		}
	}

	di.CaptureDurationSecs = int(duration.Seconds())
	return nil
}

func (di *DownloadInfo) ParseStartDelayStrVal(durationVal string) error {
	if durationVal == "" {
		return nil
	}

	// Try to parse the value as a duration string
	duration, err := str2duration.ParseDuration(durationVal)
	if err != nil {
		// Try to parse the value as a HH:MM:SS string
		duration, err = hhmmss.Parse(durationVal)
		if err != nil {
			LogError("Unable to parse value as either a Duration or a Time String: %v", err)
			return errors.New("invalid duration string")
		}
	}

	di.StartDelaySecs = int(duration.Seconds())
	return nil
}

func (di *DownloadInfo) GetCodecPriorityOrder() []string {
	baseOrder := []string{"av1", "vp9", "h264"}
	preferred := make([]string, 0, len(baseOrder))

	for _, codec := range baseOrder {
		switch codec {
		case "h264":
			if di.H264 {
				preferred = append(preferred, codec)
			}
		case "vp9":
			if di.VP9 {
				preferred = append(preferred, codec)
			}
		case "av1":
			if di.AV1 {
				preferred = append(preferred, codec)
			}
		}
	}

	order := append([]string{}, preferred...)
	for _, codec := range baseOrder {
		if !Contains(order, codec) {
			order = append(order, codec)
		}
	}

	return order
}

func (di *DownloadInfo) SelectDownloadFormats(dlUrls map[int]string, selectedQualities []string) bool {
	if len(dlUrls) == 0 {
		LogError("No download URLs found")
		return false
	}

	if di.Quality < 0 {
		var qualities []string
		qualities = append(qualities, "audio_only")
		found := false

		for _, qlabel := range VideoQualities {
			videoItag := VideoLabelItags[qlabel]
			_, vp9Ok := dlUrls[videoItag.VP9]
			_, h264Ok := dlUrls[videoItag.H264]
			_, av1Ok := dlUrls[videoItag.AV1]

			// If this label is a 60fps variant but its AV1 itag is actually the
			// same as the base-quality AV1, and the base quality has H264/VP9
			// available, treat the 60fps AV1 as not present so we fall back to
			// the base quality selection.
			if strings.HasSuffix(qlabel, "60") {
				baseQuality := strings.TrimSuffix(qlabel, "60")
				if baseItag, ok := VideoLabelItags[baseQuality]; ok {
					if baseItag.AV1 == videoItag.AV1 {
						_, baseH264Ok := dlUrls[baseItag.H264]
						_, baseVp9Ok := dlUrls[baseItag.VP9]
						if baseH264Ok || baseVp9Ok {
							av1Ok = false
						}
					}
				}
			}

			if Contains(qualities, qlabel) || (!vp9Ok && !h264Ok && !av1Ok) {
				continue
			}
			qualities = append(qualities, qlabel)
		}

		for !found {
			if len(selectedQualities) == 0 {
				selectedQualities = GetQualityFromUser(qualities, false)
			}

			for _, q := range selectedQualities {
				q = strings.TrimSpace(q)

				if q == "best" {
					q = qualities[len(qualities)-1]
				} else if q == "audio" {
					q = "audio_only"
				}

				videoItag := VideoLabelItags[q]
				aonly := videoItag.VP9 == AudioOnlyQuality

				if !di.VideoOnly {
					di.SetDownloadUrl(DtypeAudio, dlUrls[AudioItag])
				}

				if aonly {
					di.Quality = AudioOnlyQuality
					di.SetDownloadUrl(DtypeVideo, "")
					found = true
					break
				}

				codecOrder := di.GetCodecPriorityOrder()
				LogDebug("Codec priority order: %s", strings.ToUpper(strings.Join(codecOrder, ", ")))
				for _, codec := range codecOrder {
					var itag int
					switch codec {
					case "h264":
						itag = videoItag.H264
					case "vp9":
						itag = videoItag.VP9
					case "av1":
						itag = videoItag.AV1
					}
					if itag == AudioOnlyQuality {
						continue
					}
					// If the base quality has H264/VP9 available, treat the 60fps AV1 as unavailable.
					if codec == "av1" && strings.HasSuffix(q, "60") {
						if _, av1Ok := dlUrls[videoItag.AV1]; av1Ok {
							baseQuality := strings.TrimSuffix(q, "60")
							if baseItag, ok := VideoLabelItags[baseQuality]; ok {
								if baseItag.AV1 == videoItag.AV1 {
									_, baseH264Ok := dlUrls[baseItag.H264]
									_, baseVp9Ok := dlUrls[baseItag.VP9]
									if baseH264Ok || baseVp9Ok {
										LogDebug("Treating %s AV1 itag=%d as unavailable", q, videoItag.AV1)
										continue
									}
								}
							}
						}
					}
					url, ok := dlUrls[itag]
					if !ok {
						continue
					}

					di.SetDownloadUrl(DtypeVideo, url)
					di.Quality = itag
					found = true
					LogGeneral("Selected quality: %s (%s)\n", q, strings.ToUpper(codec))
					LogTrace("Video URL: %s", url)
					break
				}
				if found {
					break
				}
			}

			/*
				None of the qualities the user gave were available
				Should only be possible if they chose to wait for a stream
				and chose only qualities that the streamer ended up not using
				i.e. 1080p60/720p60 when the stream is only available in 30 FPS
			*/
			if !found {
				LogGeneral("The qualities you selected ended up unavailable for this stream")
				LogGeneral("You will now have the option to select from the available qualities")
				selectedQualities = selectedQualities[len(selectedQualities):]
			}
		}
	} else {
		aonly := di.Quality == AudioOnlyQuality
		_, audioOk := dlUrls[AudioItag]

		if !di.VideoOnly && audioOk && IsFragmented(dlUrls[AudioItag]) {
			di.SetDownloadUrl(DtypeAudio, dlUrls[AudioItag])
		}

		if !aonly {
			_, vidOk := dlUrls[di.Quality]
			if vidOk && IsFragmented(dlUrls[di.Quality]) {
				di.SetDownloadUrl(DtypeVideo, dlUrls[di.Quality])
			}
		}
	}

	if !di.VideoOnly && !di.AudioOnly && len(di.GetDownloadUrl(DtypeAudio)) == 0 {
		LogError("No audio download URL selected")
		return false
	}
	if !di.AudioOnly && len(di.GetDownloadUrl(DtypeVideo)) == 0 {
		LogError("No video download URL selected")
		return false
	}

	return true
}

func (di *DownloadInfo) shouldUpdateVideoInfo() bool {
	di.RLock()
	defer di.RUnlock()

	if di.GVideoDDL || di.Stopping || di.Unavailable {
		return false
	}

	return time.Since(di.LastUpdated) >= (DefaultPollTime * time.Second)
}

func (di *DownloadInfo) waitForYtdlpRetry(interval time.Duration) bool {
	if interval <= 0 {
		return !di.IsStopping()
	}

	timer := time.NewTimer(interval)
	defer timer.Stop()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-timer.C:
			return !di.IsStopping()
		case <-ticker.C:
			if di.IsStopping() {
				return false
			}
		}
	}
}

type ytdlpWaitStatus struct {
	RetryCount      int
	TotalWaited     int
	PollingAnnounce bool
}

func (di *DownloadInfo) GetVideoInfoFromYtdlp() bool {
	if !di.shouldUpdateVideoInfo() {
		return false
	}

	var ytdlpInfo *YtdlpExtraction
	waitStatus := &ytdlpWaitStatus{}
	infoRetries := 0
	retryInfo := func(message string) bool {
		if infoRetries >= 3 {
			return false
		}

		infoRetries += 1
		LogWarn("%s (Retry %d/%d)", message, infoRetries, 3)
		client.CloseIdleConnections()
		return true
	}
	failInfo := func(message string) bool {
		di.Lock()
		inProgress := di.InProgress
		if inProgress {
			di.Live = false
			di.Unavailable = true
		}
		di.Unlock()

		if inProgress {
			LogWarn("%s", message)
		} else {
			LogError("%s", message)
		}
		return false
	}
	for {
		for {
			jsonData := di.ExecuteYtdlpWithRetry(3)
			di.Lock()
			di.LastUpdated = time.Now()
			if jsonData == nil {
				di.Live = false
				di.Unavailable = true
				di.Unlock()
				return false
			}
			stopping := di.Stopping
			di.Unlock()
			if stopping {
				return false
			}

			var err error
			ytdlpInfo, err = di.ParseYtdlpJsonInfo(jsonData)
			if err != nil {
				message := fmt.Sprintf("Failed to parse yt-dlp JSON: %s", err)
				if retryInfo(message) {
					continue
				}
				return failInfo(message)
			}

			if !ytdlpInfo.IsUpcoming() {
				break
			}

			di.Lock()
			if !di.InfoPrinted {
				if ytdlpInfo.Channel != "" {
					LogGeneral("Channel: %s\n", ytdlpInfo.Channel)
				}
				if title := ytdlpInfo.DisplayTitle(); title != "" {
					LogGeneral("Video Title: %s\n", title)
				}
				di.InfoPrinted = true
			}
			di.Unlock()

			interval, ok := di.YtdlpWaitInterval(ytdlpInfo)
			if !ok {
				return false
			}

			seconds := int(interval.Seconds())
			if seconds < 1 {
				seconds = 1
			}

			polling := true
			if di.RetrySecs > 0 {
				if !waitStatus.PollingAnnounce {
					LogGeneral("Waiting for stream, retrying every %d seconds...\n", seconds)
					waitStatus.PollingAnnounce = true
				}
			} else if ytdlpInfo.StartUnix > 0 {
				if time.Until(time.Unix(ytdlpInfo.StartUnix, 0)) > 0 {
					LogGeneral("Stream starts at %s in %d seconds. ", formatLocalUnixTimestamp(ytdlpInfo.StartUnix), seconds)
					LogGeneral("Waiting for this time to elapse...")
					polling = false
				} else if !waitStatus.PollingAnnounce {
					LogGeneral("Stream should have started. Checking back every %d seconds\n", seconds)
					waitStatus.PollingAnnounce = true
				}
			} else {
				LogGeneral("Waiting %s before retrying yt-dlp...", SecondsToDurationAndTimeStr(seconds))
			}

			if !di.waitForYtdlpRetry(interval) {
				return false
			}
			if polling {
				waitStatus.RetryCount += 1
				waitStatus.TotalWaited += int(interval.Seconds())
				LogRetryStatus(waitStatus.RetryCount, waitStatus.TotalWaited)
			}
		}

		di.Lock()
		if !di.InfoPrinted {
			if ytdlpInfo.Channel != "" {
				LogGeneral("Channel: %s\n", ytdlpInfo.Channel)
			}
			if title := ytdlpInfo.DisplayTitle(); title != "" {
				LogGeneral("Video Title: %s\n", title)
			}
			di.InfoPrinted = true
		}
		di.Unlock()

		if len(ytdlpInfo.URLs) == 0 {
			if ytdlpInfo.IsPostLive() {
				return di.FinishYtdlpLiveRefresh("Livestream has ended and is being processed. Download URLs not available.")
			}
			if ytdlpInfo.IsNotLive() {
				return failInfo("This video is not a livestream. It would be better to use yt-dlp to download it.")
			}
			if ytdlpInfo.IsProcessedVOD() {
				return di.FinishYtdlpLiveRefresh("Livestream has been processed. Use yt-dlp instead.")
			}
			if retryInfo("No fragmented download URLs found") {
				continue
			}
			return failInfo("No fragmented download URLs found")
		}
		if ytdlpInfo.LastSq < 0 {
			if ytdlpInfo.IsPostLive() {
				return di.FinishYtdlpLiveRefresh("Livestream has ended and is being processed. Download fragment range not available.")
			}
			if ytdlpInfo.IsNotLive() {
				return failInfo("This video is not a livestream. It would be better to use yt-dlp to download it.")
			}
			if ytdlpInfo.IsProcessedVOD() {
				return di.FinishYtdlpLiveRefresh("Livestream has been processed. Use yt-dlp instead.")
			}
			if retryInfo("No fragment range found") {
				continue
			}
			return failInfo("No fragment range found")
		}
		break
	}

	di.Lock()
	defer di.Unlock()

	if di.Stopping || di.Unavailable {
		return false
	}

	if ytdlpInfo.ID != "" {
		di.VideoID = ytdlpInfo.ID
	}
	if di.VideoID == "" {
		LogError("No video ID found")
		return false
	}

	di.LastSq = ytdlpInfo.LastSq

	if !di.InfoPrinted {
		if ytdlpInfo.Channel != "" {
			LogGeneral("Channel: %s\n", ytdlpInfo.Channel)
		}
		if title := ytdlpInfo.DisplayTitle(); title != "" {
			LogGeneral("Video Title: %s\n", title)
		}
		di.InfoPrinted = true
	}

	if !di.InProgress {
		if startTimestamp := formatLocalUnixTimestamp(ytdlpInfo.StartUnix); startTimestamp != "" {
			LogGeneral("Stream started at time %s", startTimestamp)
		}
	}

	if len(ytdlpInfo.AdaptiveURLs) > 0 {
		LogDebug("Loaded %d adaptive format URLs from yt-dlp", len(ytdlpInfo.AdaptiveURLs))
	}

	selectedQualities := []string{}
	if len(di.SelectedQuality) > 0 {
		selectedQualities = ParseQualitySelection(VideoQualities, di.SelectedQuality)
	}
	if !di.SelectDownloadFormats(ytdlpInfo.URLs, selectedQualities) {
		return false
	}

	if !di.InProgress {
		pageURL := di.URL
		if di.VideoID != "" {
			pageURL = fmt.Sprintf("https://www.youtube.com/watch?v=%s", di.VideoID)
		}
		di.FormatInfo.SetYtdlpInfo(ytdlpInfo, pageURL)
		di.Metadata.SetInfo(di.FormatInfo)
		di.Thumbnail = ytdlpInfo.Thumbnail
		di.InProgress = true
	}

	di.Live = ytdlpInfo.IsLive
	return true
}

// Get necessary video info such as video/audio URLs
func (di *DownloadInfo) GetVideoInfo() bool {
	if di.YtdlpInfo {
		return di.GetVideoInfoFromYtdlp()
	}

	di.Lock()
	defer di.Unlock()

	/*
		No point retrieving information if we know it's not available, or there
		is nothing useful to be gotten
	*/
	if di.GVideoDDL || di.Stopping || di.Unavailable {
		return false
	}

	// Almost nothing we care about is likely to change in 15 seconds
	delta := time.Since(di.LastUpdated)
	if delta < (DefaultPollTime * time.Second) {
		return false
	}

	retrieved, pr, selQaulities := di.GetPlayablePlayerResponse()
	di.LastUpdated = time.Now()
	if retrieved == PlayerResponseNotFound {
		di.Live = false
		di.Unavailable = true
		return false
	} else if retrieved == PlayerResponseNotUsable {
		return false
	}

	streamData := pr.StreamingData
	pmfr := pr.Microformat.PlayerMicroformatRenderer
	isLive := pmfr.LiveBroadcastDetails.IsLiveNow

	if !di.InProgress {
		LogGeneral("Stream started at time %s", formatLocalTimestamp(pmfr.LiveBroadcastDetails.StartTimestamp))
	}

	targetDur := int(streamData.AdaptiveFormats[0].TargetDurationSec)
	if targetDur > 0 {
		di.TargetDuration = targetDur
	}
	dlUrls := di.GetDownloadUrls(pr)

	if !di.SelectDownloadFormats(dlUrls, selQaulities) {
		return false
	}

	if !di.InProgress {
		di.FormatInfo.SetInfo(pr)
		di.Metadata.SetInfo(di.FormatInfo)
		if len(pmfr.Thumbnail.Thumbnails) > 0 {
			di.Thumbnail = pmfr.Thumbnail.Thumbnails[0].URL
		}
		di.InProgress = true
	}

	di.Live = isLive

	return true
}

func (di *DownloadInfo) WaitForStartDelay() bool {
	if di.Live && di.StartDelaySecs > 0 {
		fragDur := float64(di.TargetDuration)
		secondsRoundedToFragLength := int(math.Ceil(float64(di.StartDelaySecs)/fragDur) * fragDur) // Rounds up to next frag interval time
		noOfFragsToSkip := secondsRoundedToFragLength / di.TargetDuration
		di.LiveFromSq = di.LastSq + noOfFragsToSkip

		LogGeneral("Waiting %s before starting to download...", SecondsToDurationAndTimeStr(secondsRoundedToFragLength))
		LogDebug("Will start from sequence %d [current is %d]", di.LiveFromSq, di.LastSq)

		time.Sleep(time.Duration(secondsRoundedToFragLength) * time.Second) // Waits for the specified length of time.

		if secondsRoundedToFragLength > DefaultPollTime {
			return di.GetVideoInfo() // Re-grab video information.
		}
	}

	return true
}

func (di *DownloadInfo) downloadFragment(state *fragThreadState, dataChan chan<- *Fragment) {
	state.Tries = 0
	state.FullRetries = 3
	state.Is403 = false
	fname := fmt.Sprintf("%s.frag%d.ts", state.BaseFilePath, state.SeqNum)

	for state.Tries < int(di.FragMaxTries) || di.FragMaxTries == 0 {
		if di.IsStopping() {
			return
		}
		if di.FragMaxTries == 0 {
			state.Tries = 0 // just in case someone actually somehow lets something run long enough to cause an overflow
		}

		baseUrl := di.GetDownloadUrl(state.DataType)
		seqUrl := fmt.Sprintf(baseUrl, state.SeqNum)

		req, err := http.NewRequest("GET", seqUrl, nil)
		if err != nil {
			LogDebug("%s: error creating request: %s", state.Name, err.Error())
		}

		var resp *http.Response
		dlStart := time.Now()

		if req != nil {
			host := di.GetDownloadUrlHost(state.DataType)
			if len(host) > 0 {
				req.Header.Add("Host", host)
				req.Header.Add("Referer", fmt.Sprintf("https://%s/", host))
			}

			req.Header.Add("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:152.0) Gecko/20100101 Firefox/152.0")
			req.Header.Add("Origin", "https://www.youtube.com")

			resp, err = client.Do(req)
		} else {
			resp, err = client.Get(seqUrl)
		}

		if err != nil {
			HandleFragDownloadError(di, state, err)

			state.Tries += 1
			if !ContinueFragmentDownload(di, state) {
				return
			}

			time.Sleep(state.SleepTime)
			continue
		}

		respData, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		dlDuration := time.Since(dlStart)

		if err != nil {
			HandleFragDownloadError(di, state, err)

			state.Tries += 1
			if !ContinueFragmentDownload(di, state) {
				return
			}

			time.Sleep(state.SleepTime)
			continue
		}

		if resp.StatusCode >= 400 {
			HandleFragHttpError(di, state, resp.StatusCode, baseUrl)

			state.Tries += 1
			if !ContinueFragmentDownload(di, state) {
				return
			}

			time.Sleep(state.SleepTime)
			continue
		}

		/*
			The request was a success but no data was given
			Increment the try counter and wait
		*/
		if len(respData) == 0 {
			state.Tries += 1
			if !ContinueFragmentDownload(di, state) {
				return
			}

			time.Sleep(state.SleepTime)
			continue
		}

		var data *bytes.Buffer
		headerSeqnum := -1
		headerSeqnumStr := resp.Header.Get("X-Head-Seqnum")

		if len(headerSeqnumStr) > 0 {
			headerSeqnum, _ = strconv.Atoi(headerSeqnumStr)
		}

		mimeType := resp.Header.Get("Content-Type")
		if !strings.HasSuffix(mimeType, "/mp4") && !strings.HasSuffix(mimeType, "/webm") {
			LogTrace("%s: fragment %d has unknown MIME type '%s'", state.Name, state.SeqNum, mimeType)
		}

		if state.ToFile {
			err = os.WriteFile(fname, respData, di.FileMode)
			if err != nil {
				LogDebug("%s: Failed to write fragment %d to file: %s", state.Name, state.SeqNum, err)
				di.PrintStatus()

				state.Tries += 1
				if !ContinueFragmentDownload(di, state) {
					TryDelete(fname)
					return
				}

				time.Sleep(state.SleepTime)
				continue
			}
		} else {
			data = bytes.NewBuffer(respData)
		}

		// Fragment took more than 1.5x its length to download and is not that close to the current max seq
		isSlow := false
		if headerSeqnum < 0 || state.SeqNum < (headerSeqnum-10) {
			isSlow = dlDuration > (time.Duration(float64(di.TargetDuration)*1.5) * time.Second)
		}

		dataChan <- &Fragment{
			Seq:         state.SeqNum,
			XHeadSeqNum: headerSeqnum,
			FileName:    fname,
			Data:        data,
			Slow:        isSlow,
			MimeType:    mimeType,
		}

		return
	}
}

func (di *DownloadInfo) DownloadFrags(dataType string, seqChan <-chan *seqChanInfo, dataChan chan<- *Fragment, name string) {
	defer di.DecrementJobs(dataType)
	state := NewFragThreadState(
		name,
		di.GetBaseFilePath(dataType),
		dataType,
		di.FragFiles,
		time.Duration(di.TargetDuration)*time.Second,
	)

	var endSeq int // End seq to stop on for the --capture-duration option.
	for seqInfo := range seqChan {
		if di.IsStopping() || di.IsFinished(dataType) {
			break
		}

		// --capture-duration: Stop if reaching the maximum DurationSecs.
		if di.CaptureDurationSecs != 0 {
			if endSeq == 0 {
				capSeqCnt := int(math.Ceil(float64(di.CaptureDurationSecs) / float64(di.TargetDuration)))
				endSeq = seqInfo.CurSequence + capSeqCnt // Calculate ending seq based on current seq number and DurationSecs.
			} else {
				if seqInfo.CurSequence >= endSeq {
					LogDebug("%s: Reached the maximum duration specified by --capture-duration.", name)
					di.SetFinished(dataType)
					break
				}
			}
		}

		if seqInfo.MaxSequence > -1 && !di.IsLive() && seqInfo.CurSequence >= seqInfo.MaxSequence {
			LogDebug("%s: Stream is finished and highest sequence reached", name)
			di.SetFinished(dataType)
			break
		}

		state.SeqNum = seqInfo.CurSequence
		state.MaxSeq = seqInfo.MaxSequence

		di.downloadFragment(state, dataChan)
	}

	LogDebug("%s: exiting", name)
	di.PrintStatus()
}

func (di *DownloadInfo) DownloadStream(dataType, dataFile string, progressChan chan<- *ProgressInfo, done chan<- struct{}) {
	dataChan := make(chan *Fragment, di.Jobs*2)
	seqChan := make(chan *seqChanInfo, di.Jobs*2)
	closed := false
	curFrag := 0
	startFrag := 0
	activeDownloads := 0
	maxSeqs := -1
	tries := 10
	jobNum := 1
	slowFrags := 0
	lastSlowFrag := 0
	itag := 0
	dataToWrite := make([]*Fragment, 0, di.Jobs)
	deletingFrags := make([]string, 0, 1)
	logName := fmt.Sprintf("%s-download", dataType)
	var f *os.File
	var err error
	defer func() { done <- struct{}{} }()

	if dataType == DtypeAudio {
		itag = AudioItag
	} else {
		itag = di.Quality
	}

	var resumedState bool = false
	if di.DLState[itag].Fragments > 0 {
		if di.LiveFromSq != 0 {
			if di.LiveFromVal != "" {
				LogWarn("%s: Option --live-from is being ignored as a download is being resumed.", dataType)
			}
			if di.StartDelaySecs != 0 {
				LogWarn("%s: Option --start-delay is being ignored as a download is being resumed.", dataType)
			}
		}

		f, err = os.OpenFile(dataFile, os.O_RDWR, 0666)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				LogWarn("%s: Failed to open %s to resume download: %s", dataType, dataFile, err)
				LogWarn("%s: Will truncate and start from the beginning", dataType)
			} else {
				resumedState = true
			}
			f, err = os.Create(dataFile)
		} else {
			_, err = f.Seek(di.DLState[itag].Size, 0)
			if err != nil {
				LogWarn("%s: Failed to seek %s to resume download: %s", dataType, dataFile, err)
				LogWarn("%s: Will truncate and start from the beginning", dataType)
				f, err = os.Create(dataFile)
			} else {
				resumedState = true
			}
		}
	} else {
		f, err = os.Create(dataFile)
	}

	if resumedState {
		// Resumed state: Set the startFrag and curFrag values from the state file.
		startFrag = di.DLState[itag].StartFrag
		curFrag = startFrag + di.DLState[itag].Fragments
		maxSeqs = di.LastSq
		LogInfo("%s: Resuming download from sequence %d", dataType, curFrag)
	} else {
		if di.LastSq >= 0 {
			curFrag = di.seekableStartSqFromLastSq(di.LastSq)
			maxSeqs = di.LastSq
		}

		if di.LiveFromSq != 0 {
			// --live-from or --start-delay: Set start sequence.
			curFrag = di.LiveFromSq
			startFrag = curFrag

			if di.LiveFromVal != "" {
				LogDebug("%s: Starting from sequence %d (latest is %d)", dataType, startFrag, di.LastSq)
			}
			if di.StartDelaySecs != 0 {
				LogDebug("%s: Starting from sequence %d (latest is %d)", dataType, startFrag, di.LastSq)
			}
		} else if curFrag > 0 {
			// Stream that has been live for more than 7 days.
			LogWarn("%s: YT only retains the livestream 7 days past for seeking, starting from sequence %d (latest is %d)", dataType, curFrag, di.LastSq)
			startFrag = curFrag
		} else {
			// All other stream lengths.
			curFrag = 0
		}

		di.DLState[itag].StartFrag = startFrag // Sets start frag in state file for resuming.
	}
	curSeq := curFrag

	if err != nil {
		LogError("%s: Error opening %s for writing: %s", dataType, dataFile, err)
		di.Stop()
		return
	}
	defer f.Close()

	for di.GetActiveJobCount(dataType) < di.Jobs {
		jobName := fmt.Sprintf("%s%d", dataType, jobNum)
		di.IncrementJobs(dataType)
		seqChan <- &seqChanInfo{curSeq, maxSeqs}
		curSeq += 1
		activeDownloads += 1
		jobNum += 1
		go di.DownloadFrags(dataType, seqChan, dataChan, jobName)
	}

	for {
		dataReceived := false
		downloading := di.GetActiveJobCount(dataType) > 0
		stopping := di.IsStopping()

		if stopping || !downloading || di.IsFinished(dataType) {
			if !closed {
				close(seqChan)
				closed = true
			}
		} else if slowFrags >= 10 {
			// RefreshURL(di, dataType, "")
			slowFrags = 0
		}

	getData:
		for {
			select {
			case data := <-dataChan:
				dataReceived = true
				dataToWrite = append(dataToWrite, data)
				activeDownloads -= 1

				if !downloading || stopping || closed {
					continue
				}

				if data.XHeadSeqNum > maxSeqs {
					maxSeqs = data.XHeadSeqNum
				}

				if maxSeqs > 0 {
					for (curSeq <= maxSeqs+1 && activeDownloads < di.Jobs) || activeDownloads < 1 {
						seqChan <- &seqChanInfo{curSeq, maxSeqs}
						curSeq += 1
						activeDownloads += 1
					}
				} else {
					seqChan <- &seqChanInfo{curSeq, maxSeqs}
					curSeq += 1
					activeDownloads += 1
				}

				if data.Slow {
					// Only increment if it's been less than 10 frags since the last slow one
					// Reset the counter otherwise. Should hopefully prevent getting rid of
					// an otherwise good download url
					if (data.Seq - lastSlowFrag) < 10 {
						slowFrags += 1
					} else {
						slowFrags = 1
					}

					lastSlowFrag = data.Seq
				}
			default:
				break getData
			}
		}

		if (len(dataToWrite) == 0 || !dataReceived) && downloading {
			if !stopping && activeDownloads <= 0 {
				LogDebug("%s: Somehow no active downloads and no data to write", logName)
				LogDebug("%s: Fragment this happened at: %d", logName, curFrag)
				di.PrintStatus()

				for activeDownloads < di.GetActiveJobCount(dataType) {
					seqChan <- &seqChanInfo{curSeq, maxSeqs}
					curSeq += 1
					activeDownloads += 1
				}
			}

			time.Sleep(100 * time.Millisecond)
			continue
		}

		i := 0
		for i < len(dataToWrite) && tries > 0 {
			data := dataToWrite[i]
			if data.Seq != curFrag {
				i += 1
				continue
			}

			if di.FragFiles {
				readBytes, err := os.ReadFile(data.FileName)

				if err != nil {
					tries -= 1
					LogWarn("%s: Error when attempting to read fragment %d for writing: %s", logName, curFrag, err)
					di.PrintStatus()

					if tries > 0 {
						LogWarn("%s: Will try %d more time(s)", logName, tries)
						di.PrintStatus()
					}

					continue
				}

				data.Data = bytes.NewBuffer(readBytes)
			}

			bytesWritten := 0
			buf := make([]byte, BufferSize)

			rc, _ := data.Data.Read(buf)

			writeBuf := buf
			// ffmpeg doesn't like certain atoms in concatenated MP4 files, so we remove those here
			// If MimeType is blank, assume MP4
			if strings.HasSuffix(data.MimeType, "/mp4") || data.MimeType == "" {
				badAtoms := []string{"sidx"}
				// ffmpeg 6.1 doesn't like multiple ftyp atoms, so only allow on the first fragment
				if curFrag != startFrag {
					badAtoms = append(badAtoms, "ftyp")
				}
				writeBuf = RemoveAtoms(buf[:rc], badAtoms...)
			}

			count, err := f.Write(writeBuf)
			bytesWritten += count

			if err != nil {
				tries -= 1
				LogWarn("%s: Error when attempting to write fragment %d to %s: %s", logName, curFrag, dataFile, err)
				di.PrintStatus()

				// If we errored but wrote some data, set the offset back to
				// where we want to write the fragment
				f.Seek(int64(bytesWritten), 1)

				if tries > 0 {
					LogWarn("%s: Will try %d more time(s)", logName, tries)
					di.PrintStatus()
				}

				continue
			}

			for {
				count, err = data.Data.Read(buf)
				if err != nil {
					break
				}

				count, err = f.Write(buf[:count])
				bytesWritten += count

				if err != nil {
					tries -= 1
					LogWarn("%s: Error when attempting to write fragment %d to %s: %s", logName, curFrag, dataFile, err)
					di.PrintStatus()

					f.Seek(int64(bytesWritten), 1)

					if tries > 0 {
						LogWarn("%s: Will try %d more time(s)", logName, tries)
						di.PrintStatus()
					}

					break
				}
			}

			// something didn't work
			if err != nil && err != io.EOF {
				continue
			}

			curFrag += 1
			progressChan <- &ProgressInfo{itag, bytesWritten, maxSeqs, startFrag}

			if di.FragFiles {
				err = os.Remove(data.FileName)
				if err != nil {
					LogWarn("%s: Error deleting fragment %d: %s", logName, data.Seq, err)
					LogWarn("%s: Will try again after the download has finished", logName)
					deletingFrags = append(deletingFrags, data.FileName)
					di.PrintStatus()
				}
			}

			dataToWrite = append(dataToWrite[:i], dataToWrite[i+1:]...)
			tries = 10
			i = 0
		}

		if !downloading {
			break
		}

		// updateDelta := di.GetTimeSinceUpdated()
		// if !stopping && !di.IsUnavailable() && updateDelta > time.Hour {
		// 	di.GetVideoInfo()
		// }

		if tries <= 0 {
			LogWarn("%s: Stopping download, something must be wrong...", logName)
			di.PrintStatus()
			di.Stop()
		}
	}

	if di.FragFiles {
		for _, d := range dataToWrite {
			TryDelete(d.FileName)
		}
	}

	for _, d := range deletingFrags {
		LogInfo("%s: Attempting to delete fragments that failed to be deleted before", logName)
		TryDelete(d)
	}

	LogDebug("%s thread closing", logName)
	di.PrintStatus()
}
