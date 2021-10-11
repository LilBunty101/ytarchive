package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/alessio/shellescape"
)

// DASH Manifest element containing Youtube's media ID and a download URL
type Representation struct {
	Id      string `xml:"id,attr"`
	BaseURL string
}

type Atom struct {
	Offset int
	Length int
}

const (
	LogleveError = iota
	LogleveWarning
	LogleveInfo
	LoglevelDebug
)

const (
	_           = iota
	KiB float64 = 1 << (10 * iota)
	MiB
	GiB
)

const (
	NetworkBoth         = "tcp"
	NetworkIPv4         = "tcp4"
	NetworkIPv6         = "tcp6"
	DefaultPollTime     = 15
	DefaultVideoQuality = "best"
)

var (
	HtmlVideoLinkTag = []byte(`<link rel="canonical" href="https://www.youtube.com/watch?v=`)

	loglevel              = LogleveWarning
	networkType           = NetworkBoth // Set to force IPv4 or IPv6
	networkOverrideDialer = &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	tr = &http.Transport{
		DialContext:           DialContextOverride,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
	}
	client = &http.Client{
		Transport: tr,
	}
)

var VideoQualities = []string{
	"audio_only",
	"144p",
	"240p",
	"360p",
	"480p",
	"720p",
	"720p60",
	"1080p",
	"1080p60",
}

var fnameReplacer = strings.NewReplacer(
	"<", "_",
	">", "_",
	":", "_",
	`"`, "_",
	"/", "_",
	"\\", "_",
	"|", "_",
	"?", "_",
	"*", "_",
)

/*
   Logging functions;
   ansi sgr 0=reset, 1=bold, while 3x sets the foreground color:
   0black 1red 2green 3yellow 4blue 5magenta 6cyan 7white
*/
func LogError(format string, args ...interface{}) {
	if loglevel >= LogleveError {
		msg := format
		if len(args) > 0 {
			msg = fmt.Sprintf(format, args...)
		}
		log.Printf("ERROR: \033[31m%s\033[0m\033[K", msg)
	}
}

func LogWarn(format string, args ...interface{}) {
	if loglevel >= LogleveWarning {
		msg := format
		if len(args) > 0 {
			msg = fmt.Sprintf(format, args...)
		}
		log.Printf("WARNING: \033[33m%s\033[0m\033[K", msg)
	}
}

func LogInfo(format string, args ...interface{}) {
	if loglevel >= LogleveInfo {
		msg := format
		if len(args) > 0 {
			msg = fmt.Sprintf(format, args...)
		}
		log.Printf("INFO: \033[32m%s\033[0m\033[K", msg)
	}
}

func LogDebug(format string, args ...interface{}) {
	if loglevel >= LoglevelDebug {
		msg := format
		if len(args) > 0 {
			msg = fmt.Sprintf(format, args...)
		}
		log.Printf("DEBUG: \033[36m%s\033[0m\033[K", msg)
	}
}

func DialContextOverride(ctx context.Context, network, addr string) (net.Conn, error) {
	return networkOverrideDialer.DialContext(ctx, networkType, addr)
}

// Remove any illegal filename chars
func SterilizeFilename(s string) string {
	return fnameReplacer.Replace(s)
}

// Pretty formatting of byte count
func FormatSize(bsize int64) string {
	bsFloat := float64(bsize)

	switch {
	case bsFloat >= GiB:
		return fmt.Sprintf("%.2fGiB", bsFloat/GiB)
	case bsFloat >= MiB:
		return fmt.Sprintf("%.2fMiB", bsFloat/MiB)
	case bsFloat >= KiB:
		return fmt.Sprintf("%.2fKiB", bsFloat/KiB)
	}
	return fmt.Sprintf("%dB", bsize)
}

/*
	This is pretty dumb but the only way to handle sigint in a custom way
	Thankfully we don't call this often enough to really care
*/
func getInput(c chan<- string) {
	var input string
	scanner := bufio.NewScanner(os.Stdin)

	if scanner.Scan() {
		input = strings.TrimSpace(scanner.Text())
	}

	c <- input
}

func GetUserInput(prompt string) string {
	var input string
	inputChan := make(chan string)
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)
	defer signal.Reset(os.Interrupt)

	fmt.Print(prompt)
	go getInput(inputChan)

	select {
	case input = <-inputChan:
	case <-sigChan:
		fmt.Println("\nExiting...")
		os.Exit(1)
	}

	return input
}

func GetYesNo(prompt string) bool {
	yesno := GetUserInput(fmt.Sprintf("%s [y/N]: ", prompt))
	yesno = strings.ToLower(yesno)

	return strings.HasPrefix(yesno, "y")
}

/*
   Execute an external process using the given args
   Returns the process return code, or -1 on unknown error
*/
func Execute(prog string, args []string) int {
	retcode := 0
	cmd := exec.Command(prog, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	LogDebug("Executing command: %s", shellescape.QuoteCommand(cmd.Args))

	err := cmd.Run()
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			retcode = cmd.ProcessState.ExitCode()
			LogError(stderr.String())
		} else {
			retcode = -1
			LogError(err.Error())
		}
	}

	return retcode
}

// Download data from the given URL
func DownloadData(url string) []byte {
	var data []byte

	resp, err := client.Get(url)
	if err != nil {
		LogWarn("Failed to retrieve data from %s: %v", url, err)
		return data
	}
	defer resp.Body.Close()

	data, err = io.ReadAll(resp.Body)
	if err != nil {
		LogWarn("Failed to retrieve data from %s: %v", url, err)
		return data
	}

	return data
}

/*
	Download the given url to the given file name.
	Obviously meant to be used for thumbnail images.
*/
func DownloadThumbnail(url, fname string) bool {
	resp, err := client.Get(url)
	if err != nil {
		LogWarn("Failed to download thumbnail: %v", err)
		return false
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		LogWarn("Failed to download thumbnail: %v", err)
		return false
	}

	err = os.WriteFile(fname, data, 0644)
	if err != nil {
		LogWarn("Failed to write thumbnail: %v", err)
		os.Remove(fname)
		return false
	}

	return true
}

// Make a comma-separated list of available formats
func MakeQualityList(formats []string) string {
	var sb strings.Builder

	for _, v := range formats {
		fmt.Fprintf(&sb, "%s, ", v)
	}

	sb.WriteString("best")
	return sb.String()
}

// Parse the user-given list of qualities they are willing to accept for download
func ParseQualitySelection(formats []string, quality string) []string {
	var selQualities []string
	quality = strings.ToLower(strings.TrimSpace(quality))
	qualities := strings.Split(quality, "/")

	for _, q := range qualities {
		stripped := strings.TrimSpace(q)

		if stripped == "best" {
			selQualities = append(selQualities, stripped)
			continue
		}

		for _, v := range formats {
			if stripped == v {
				selQualities = append(selQualities, stripped)
				break
			}
		}
	}

	if len(selQualities) < 1 {
		fmt.Println("No valid qualities selected")
	}

	return selQualities
}

// Prompt the user to select a video quality
func GetQualityFromUser(formats []string, waiting bool) []string {
	var selQualities []string
	qualities := MakeQualityList(formats)

	if waiting {
		fmt.Printf("%s\n%s\n%s\n\n",
			"Since you are going to wait for the stream, you must pre-emptively select a video quality.",
			"There is no way to know which qualities will be available before the stream starts, so a list of all possible stream qualities will be presented.",
			"You can use youtube-dl style selection (slash-delimited first to last preference). Default is 'best'",
		)
	}

	fmt.Printf("Available video qualities: %s\n", qualities)

	for len(selQualities) < 1 {
		quality := GetUserInput("Enter desired video quality: ")
		quality = strings.ToLower(quality)
		if len(quality) == 0 {
			quality = DefaultVideoQuality
		}

		selQualities = ParseQualitySelection(formats, quality)
	}

	return selQualities
}

/*
	Per anon, there will be a noclen parameter if the given URLs are meant to
	be downloaded in fragments. Else it will have a clen parameter, obviously
	specifying content length.
*/
func IsFragmented(url string) bool {
	return strings.Index(strings.ToLower(url), "noclen") > 0
}

// Prase the DASH manifest XML and get the download URLs from it
func GetUrlsFromManifest(manifest []byte) map[int]string {
	urls := make(map[int]string)
	var reps []Representation

	err := xml.Unmarshal(manifest, &reps)
	if err != nil {
		LogWarn("Error parsing DASH manifest: %s", err)
		return urls
	}

	for _, r := range reps {
		itag, err := strconv.Atoi(r.Id)
		if err != nil {
			continue
		}

		if itag > 0 && len(r.BaseURL) > 0 {
			urls[itag] = strings.ReplaceAll(r.BaseURL, "%", "%%") + "sq/%d"
		}
	}

	return urls
}

func StringsIndex(arr []string, s string) int {
	for i := 0; i < len(arr); i++ {
		if arr[i] == s {
			return i
		}
	}

	return -1
}

// https://stackoverflow.com/a/61822301
func InsertStringAt(arr []string, idx int, s string) []string {
	if len(arr) == idx {
		return append(arr, s)
	}

	arr = append(arr[:idx+1], arr[idx:]...)
	arr[idx] = s
	return arr
}

func GetAtoms(data []byte) map[string]Atom {
	atoms := make(map[string]Atom)
	ofs := 0

	for {
		if ofs+8 > len(data) {
			break
		}

		lenHex := hex.Dump(data[ofs : ofs+4])
		aLen, err := strconv.ParseInt(lenHex, 16, 0)

		if err != nil || int(aLen) > len(data) {
			break
		}

		aName := string(data[ofs+4 : ofs+8])
		atoms[aName] = Atom{Offset: ofs, Length: int(aLen)}
		ofs += int(aLen)
	}

	return atoms
}

func RemoveSidx(data []byte) []byte {
	atoms := GetAtoms(data)
	sidx, ok := atoms["sidx"]

	if !ok {
		return data
	}

	ofs := sidx.Offset
	rlen := sidx.Offset + sidx.Length
	newData := append(data[:ofs], data[rlen:]...)

	return newData
}

func GetVideoIdFromWatchPage(data []byte) string {
	startIdx := bytes.Index(data, HtmlVideoLinkTag)
	if startIdx < 0 {
		return ""
	}

	startIdx += len(HtmlVideoLinkTag)
	endIdx := bytes.Index(data[startIdx:], []byte(`"`)) + startIdx

	return string(data[startIdx:endIdx])
}

func ParseGvideoUrl(gvUrl, dataType string) (string, int) {
	var newUrl string
	parsedUrl, err := url.Parse(gvUrl)
	if err != nil {
		LogError("Error parsing Google Video URL: %s", err)
		return newUrl, 0
	}

	lowerHost := strings.ToLower(parsedUrl.Hostname())
	sqIndex := strings.Index(gvUrl, "&sq=")

	itag, err := strconv.Atoi(parsedUrl.Query().Get("itag"))
	if err != nil {
		LogError("Error parsing itag in Google Video URL: %s", err)
		return newUrl, 0
	}

	if !strings.HasSuffix(lowerHost, ".googlevideo.com") {
		return newUrl, 0
	} else if _, ok := parsedUrl.Query()["noclen"]; !ok {
		fmt.Println("Given Google Video URL is not for a fragmented stream.")
		return newUrl, 0
	} else if dataType == DtypeAudio && itag != AudioItag {
		fmt.Println("Given audio URL does not have the audio itag. Make sure you set the correct URL(s)")
		return newUrl, 0
	} else if dataType == DtypeVideo && itag == AudioItag {
		fmt.Println("Given video URL has the audio itag set. Make sure you set the correct URL(s)")
		return newUrl, 0
	} else if sqIndex < 0 {
		fmt.Println("Given video URL did not have a sequence parameter.")
		return newUrl, 0
	}

	newUrl = gvUrl[:sqIndex] + "&sq=%s"
	return newUrl, itag
}

func RefreshURL(di *DownloadInfo, dataType, currentUrl string) {
	if !di.IsGVideoDDL() {
		newUrl := di.GetDownloadUrl(dataType)

		if len(currentUrl) == 0 || newUrl == currentUrl {
			LogDebug("%s: Attempting to retrieve a new download URL", dataType)
			di.PrintStatus()

			di.GetVideoInfo()
		}
	}
}

func ContinueFragmentDownload(di *DownloadInfo, state *fragThreadState) bool {
	if di.IsFinished(state.DataType) {
		return false
	}

	if state.Tries >= FragMaxTries {
		state.FullRetries -= 1

		LogDebug("%s: Fragment %d: %d/%d retries", state.Name, state.SeqNum, state.Tries, FragMaxTries)
		di.PrintStatus()

		// Update video info to be safe if we are known to still be live
		if di.IsLive() {
			di.GetVideoInfo()
		}

		if !di.IsLive() {
			if di.IsUnavailable() && state.Is403 {
				LogWarn("%s: Download link likely expired and stream is privated or members only, cannot coninue download", state.Name)
				di.PrintStatus()
				di.SetFinished(state.DataType)
				return false
			} else if state.MaxSeq > -1 && state.SeqNum < (state.MaxSeq-2) && state.FullRetries > 0 {
				LogDebug("%s: More than two fragments away from the highest known fragment", state.Name)
				LogDebug("%s: Will try grabbing the fragment %d more times", state.Name, state.FullRetries)
				di.PrintStatus()
			} else {
				di.SetFinished(state.DataType)
				return false
			}
		} else {
			LogDebug("%s: Fragment %d: Stream still live, continuing download attempt", state.Name, state.SeqNum)
			di.PrintStatus()
			state.Tries = 0
		}
	}

	return true
}

func HandleFragHttpError(di *DownloadInfo, state *fragThreadState, statusCode int, url string) {
	LogDebug("%s: HTTP Error for fragment %d: %d %s", state.Name, state.SeqNum, statusCode, http.StatusText(statusCode))
	di.PrintStatus()

	if statusCode == http.StatusForbidden {
		state.Is403 = true
		RefreshURL(di, state.DataType, url)
	} else if statusCode == http.StatusNotFound && state.MaxSeq > -1 && !di.IsLive() && state.SeqNum > (state.MaxSeq-2) {
		LogDebug("%s: Stream has ended and fragment within the last two nor found, probably not actually created", state.Name)
		di.PrintStatus()
		di.SetFinished(state.DataType)
	}
}

func HandleFragDownloadError(di *DownloadInfo, state *fragThreadState, err error) {
	LogDebug("%s: Error with fragment %d: %s", state.Name, state.SeqNum, err)
	di.PrintStatus()

	if state.MaxSeq > -1 && !di.IsLive() && state.SeqNum >= (state.MaxSeq-2) {
		LogDebug("%s: Stream has ended and fragment number is within two of the known max, probably not actually created", state.Name)
		di.SetFinished(state.DataType)
		di.PrintStatus()
	}
}

func TryMove(srcFile, dstFile string) {
	_, err := os.Stat(srcFile)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			LogWarn("Error moving file: %s", err)
		}

		return
	}

	LogInfo("Moving file %s to %s", srcFile, dstFile)

	err = os.Rename(srcFile, dstFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		LogWarn("Error moving file: %s", err)
	}
}

func TryDelete(fname string) {
	_, err := os.Stat(fname)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			LogWarn("Error deleting file: %s", err)
		}

		return
	}

	LogInfo("Deleting file %s", fname)
	err = os.Remove(fname)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		LogWarn("Error deleting file: %s", err)
	}
}

// Call os.State and check if err is os.ErrNotExist
// Unsure if the file is guaranteed to exist when err is not nil or os.ErrNotExist
func Exists(file string) bool {
	_, err := os.Stat(file)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false
		}
	}

	return true
}

func CleanupFiles(files []string) {
	for _, f := range files {
		TryDelete(f)
	}
}

// Very dirty Python string formatter. Requires map keys i.e. "%(key)s"
// Throws an error if a map key is not in vals.
// This is NOT how to do a parser haha
func FormatPythonMapString(format string, vals map[string]string) (string, error) {
	pythonMapKey := regexp.MustCompile(`%\((\w+)\)s`)

	for {
		match := pythonMapKey.FindStringSubmatch(format)
		if match == nil {
			return format, nil
		}

		key := strings.ToLower(match[1])
		if _, ok := vals[key]; !ok {
			return "", fmt.Errorf("unknown output format key: '%s'", key)
		}

		val := vals[key]
		format = strings.ReplaceAll(format, match[0], val)
	}
}

func FormatFilename(format string, vals map[string]string) (string, error) {
	fnameVals := make(map[string]string)

	for k, v := range vals {
		if Contains(FilenameFormatBlacklist, k) {
			fnameVals[k] = ""
		}

		fnameVals[k] = SterilizeFilename(v)
	}

	return FormatPythonMapString(format, fnameVals)
}

// Case insensitive search. Naive linear
func Contains(arr []string, val string) bool {
	val = strings.ToLower(strings.TrimSpace(val))

	for _, s := range arr {
		if strings.ToLower(strings.TrimSpace(s)) == val {
			return true
		}
	}

	return false
}