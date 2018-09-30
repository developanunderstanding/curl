package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/jessevdk/go-flags"
	"github.com/juju/ratelimit"
	"github.com/pkg/errors"
)

var args Arguments

// Arguments command line arguments
type Arguments struct {
	Data             *string `short:"d" long:"data" description:"HTTP POST data" value-name:"DATA"`
	PostWithGet      bool    `short:"G" long:"get" description:"Send -d DATA with HTTP GET"`
	OutputFile       *string `short:"o" long:"output" description:"Write to FILE instead of stdout" value-name:"FILE"`
	UseRemoteName    bool    `short:"O" long:"remote-name" description:"Write output to a file named as the remote file"`
	OutputStream     *os.File
	HTTPMethod       string            `short:"X" long:"request" description:"Specify request command to use" value-name:"COMMAND"`
	Headers          map[string]string `short:"H" long:"header" description:"Pass custom header LINE to server" value-name:"LINE"`
	Information      bool              `short:"I" long:"head" description:"Show document info only"`
	LimitRate        *string           `long:"limit-rate" description:"limit transfer speed to RATE" value-name:"RATE"`
	LimitRateBytes   int64
	URL              string
	MaxDownload      *string `long:"max-filesize" description:"Maximum filesize to download" value-name:"BYTES"`
	MaxDownloadBytes int64
	Verbose          []bool `short:"v" long:"verbose"`
}

// parseFlags parses and transforms arguments
func parseFlags() {
	unused, err := flags.Parse(&args)
	// unused, err := flags.ParseArgs(&args, []string{
	// 	"https://beatthehash.com/results", "-O",
	// 	"--max-filesize", "11K",
	// 	// "https://postman-echo.com/post",
	// })
	if flags.WroteHelp(err) {
		// asked for help? Exit!
		os.Exit(0)
	}
	if err != nil {
		panic(err)
	}

	// URLs don't have flags, are unused
	if len(unused) != 0 {
		args.URL = unused[0]
	} else {
		// no unused parameters means no URL
		panic(errors.New("no URL specified"))
	}

	if strings.Index(args.URL, "://") < 0 {
		// protocol is unmentioned, assume HTTP
		args.URL = "http://" + args.URL
	}

	// Determine HTTP Method from arguments if unspecified
	if args.HTTPMethod == "" {
		if args.Information {
			args.HTTPMethod = "HEAD"
		} else if args.Data != nil && !args.PostWithGet {
			args.HTTPMethod = "POST"
		}
	} else {
		args.HTTPMethod = "GET"
	}

	// if data begins with '@', assume filename and use file data
	if args.Data != nil && (*args.Data)[0] == '@' {
		filename := (*args.Data)[1:]
		var raw []byte
		raw, err = ioutil.ReadFile(filename)
		if err != nil {
			panic(err)
		}
		*args.Data = string(raw)
	}

	// Check if certain headers are specified by user
	isContentTypeSpecified := false
	isContentLengthSpecified := false
	for key := range args.Headers {
		switch strings.ToUpper(key) {
		case "CONTENT-TYPE":
			isContentTypeSpecified = true
			break
		case "CONTENT-LENGTH":
			isContentLengthSpecified = true
			break
		}
	}

	// fill in content headers if unspecified
	if !isContentTypeSpecified {
		if args.Data != nil {
			contentType, requiresHeader := guessDataType(*args.Data)
			if requiresHeader {
				args.Headers["Content-Type"] = contentType
			}
		}
	}
	if args.Data != nil && !isContentLengthSpecified {
		args.Headers["Content-Length"] = strconv.Itoa(len(*args.Data))
	}

	if args.LimitRate != nil {
		args.LimitRateBytes, err = parseSize(*args.LimitRate)
		if err != nil {
			panic(err)
		}
	}

	if args.MaxDownload != nil {
		args.MaxDownloadBytes, err = parseSize(*args.MaxDownload)
		if err != nil {
			panic(err)
		}
	}
}

func main() {
	parseFlags()

	// construct request
	var req *http.Request
	var err error
	if args.Data != nil {
		// if there's a body...
		body := bytes.NewBufferString(*args.Data)
		if args.LimitRate != nil {
			// ... and we're rate limiting
			bucket := ratelimit.NewBucketWithRate(float64(args.LimitRateBytes), args.LimitRateBytes)
			limiter := ratelimit.Reader(body, bucket)
			req, err = http.NewRequest(args.HTTPMethod, args.URL, limiter)
		} else {
			// ... and we're NOT rate limiting
			req, err = http.NewRequest(args.HTTPMethod, args.URL, body)
		}
	} else {
		// if there's no body
		req, err = http.NewRequest(args.HTTPMethod, args.URL, nil)
	}
	if err != nil {
		panic(err)
	}

	// add headers
	for key, value := range args.Headers {
		req.Header.Add(key, strings.TrimSpace(value))
	}

	// print request info if verbose is set
	if len(args.Verbose) >= 1 {
		fmt.Printf("%s %s %s\n", req.Proto, req.Method, req.URL)
		for key, values := range req.Header {
			for _, value := range values {
				fmt.Printf("%s: %s\n", key, value)
			}
		}
		if args.OutputFile == nil && args.OutputStream == nil {
			// print extra line between headers and response
			fmt.Println()
		}
	}

	// make the request
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	defer res.Body.Close()

	// prepare output stream for file or stdout
	if args.OutputFile == nil {
		if args.UseRemoteName {
			disposition := res.Header.Get("Content-Disposition")
			if disposition != "" {
				pattern := regexp.MustCompile(`filename="?(.+)"?`)
				matches := pattern.FindStringSubmatch(disposition)
				if len(matches) >= 2 {
					args.OutputFile = new(string)
					*args.OutputFile = matches[1]
				}
			} else {
				args.OutputFile = new(string)
				*args.OutputFile = path.Base(args.URL)
			}
		} else {
			args.OutputStream = os.Stdout
		}
	}
	if args.OutputFile != nil {
		args.OutputStream, err = os.OpenFile(*args.OutputFile, os.O_CREATE|os.O_WRONLY, 0666)
		if err != nil {
			panic(err)
		}
		defer args.OutputStream.Close()
	}

	// read response, write to output
	buffer := make([]byte, 1024)
	var n int
	var total int64
	for {
		n, err = res.Body.Read(buffer)
		if err != nil && err != io.EOF {
			panic(err)
		}
		if args.MaxDownload != nil && total+int64(n) >= args.MaxDownloadBytes {
			// limit based on download size
			n = int(args.MaxDownloadBytes - total)
			err = io.EOF
		}
		total += int64(n)
		_, err2 := args.OutputStream.Write(buffer[:n])
		if err2 != nil {
			panic(err2)
		}

		if err == io.EOF {
			break
		}
	}
	// All done!
}

// guessDataType checks for json or form data
func guessDataType(data string) (contentType string, requiresHeader bool) {
	temp := json.RawMessage{}
	err := json.NewDecoder(bytes.NewBufferString(data)).Decode(&temp)
	if err == nil {
		return "application/json", true
	}

	pattern := regexp.MustCompile(`.*?=.+?&?\b`)
	if pattern.Match([]byte(data)) {
		return "application/x-www-form-urlencoded", true
	}

	return "", false
}

// parseSize checks for human readable sizes and converts to byte count
func parseSize(value string) (size int64, err error) {
	if strings.ContainsRune("kKmMgG", rune((value)[len(value)-1])) {
		multiplier := (value)[len(value)-1]
		number, err := strconv.ParseInt((value)[:len(value)-1], 10, 64)
		if err != nil {
			return -1, err
		}
		switch multiplier {
		case 'k': //kilobyte
		case 'K':
			number *= 1024
			break
		case 'm': // megabyte
		case 'M':
			number *= 1024 * 1024
		case 'g': // gigabyte
		case 'G':
			number *= 1024 * 1024 * 1024
		}
		return number, nil
	}
	// no multiplier character, assume raw byte count
	return strconv.ParseInt(value, 10, 64)
}
