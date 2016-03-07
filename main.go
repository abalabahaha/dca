package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/layeh/gopus"
)

// Define constants
const (
	// The current version of the DCA format
	FormatVersion int8 = 1

	// The current version of the DCA program
	ProgramVersion string = "0.0.1"

	// The URL to the GitHub repository of DCA
	GitHubRepositoryURL string = "https://github.com/bwmarrin/dca"
)

// All global variables used within the program
var (
	// Buffer for some commands
	CmdBuf bytes.Buffer
	PngBuf bytes.Buffer

	CoverImage string

	// Metadata structures
	Metadata    MetadataStruct
	FFprobeData FFprobeMetadata

	// Magic bytes to write at the start of a DCA file
	MagicBytes string = fmt.Sprintf("DCA%d", FormatVersion)

	// 1 for mono, 2 for stereo
	Channels int

	// Must be one of 8000, 12000, 16000, 24000, or 48000.
	// Discord only uses 48000 currently.
	FrameRate int

	// Rates from 500 to 512000 bits per second are meaningful
	// Discord only uses 8000 to 128000 and default is 64000
	Bitrate int

	// Must be one of voip, audio, or lowdelay.
	// DCA defaults to audio which is ideal for music
	// Not sure what Discord uses here, probably voip
	Application string

	// if true, dca sends raw output without any magic bytes or json metadata
	RawOutput bool

	FrameSize int // uint16 size of each audio frame
	MaxBytes  int // max size of opus data

	Volume int // change audio volume (256=normal)

	DCAMode string // decode or encode

	OpusDecoder *gopus.Decoder
	OpusEncoder *gopus.Encoder

	InFile      string
	IsUrl       bool   // is InFile a url
	CoverFormat string = "jpeg"

	OutFile string = "pipe:1"
	OutBuf  []byte

	DecodeInputChan chan []byte
	DecodeOutputChan chan []int16

	EncodeInputChan chan []int16
	EncodeOutputChan chan []byte

	err error

	wg sync.WaitGroup
)

// init configures and parses the command line arguments
func init() {

	flag.StringVar(&InFile, "i", "pipe:0", "infile")
	flag.StringVar(&OutFile, "o", "pipe:1", "output file")
	flag.IntVar(&Volume, "vol", 256, "change audio volume (256=normal)")
	flag.IntVar(&Channels, "ac", 2, "audio channels")
	flag.IntVar(&FrameRate, "ar", 48000, "audio sampling rate")
	flag.IntVar(&FrameSize, "as", 960, "audio frame size can be 960 (20ms), 1920 (40ms), or 2880 (60ms)")
	flag.IntVar(&Bitrate, "ab", 64, "audio encoding bitrate in kb/s can be 8 - 128")
	flag.BoolVar(&RawOutput, "raw", false, "Raw opus output (no metadata or magic bytes)")
	flag.StringVar(&Application, "aa", "audio", "audio application can be voip, audio, or lowdelay")
	flag.StringVar(&CoverFormat, "cf", "jpeg", "format the cover art will be encoded with")
	flag.StringVar(&DCAMode, "mode", "", "specify whether to encode (default) or decode")

	if len(os.Args) < 2 {
		flag.Usage()
		os.Exit(1)
	}

	flag.Parse()

	MaxBytes = (FrameSize * Channels) * 2 // max size of opus data
}

// very simple program that wraps ffmpeg and outputs raw opus data frames
// with a uint16 header for each frame with the frame length in bytes
func main() {

	//////////////////////////////////////////////////////////////////////////
	// BLOCK : Basic setup and validation
	//////////////////////////////////////////////////////////////////////////

	// If only one argument provided assume it's a filename.
	if len(os.Args) == 2 {
		InFile = os.Args[1]
	}

	IsUrl = strings.HasPrefix(InFile, "http://") || strings.HasPrefix(InFile, "https://")

	if !IsUrl && InFile == "" && strings.HasSuffix(InFile, ".dca") {
		DCAMode = "decode"
	}

	//////////////////////////////////////////////////////////////////////////
	// BLOCK : Create chans, buffers, and encoder for use
	//////////////////////////////////////////////////////////////////////////
	// If reading from pipe, make sure pipe is open
	if InFile == "pipe:0" {
		fi, err := os.Stdin.Stat()
		if err != nil {
			fmt.Println(err)
			return
		}

		if (fi.Mode() & os.ModeCharDevice) != 0 {
			fmt.Println("Error: stdin is not a pipe.")
			flag.Usage()
			return
		}
	} else if IsUrl {
		// If reading from a URL, verify the URL is valid.
		resp, err := http.Get(InFile)
		if err != nil {
			fmt.Println("HTTP Request Error: ", err)
			return
		}
		if resp.StatusCode != 200 {
			fmt.Printf("Error: Requesting URL returned HTTP error code %d\n", resp.StatusCode)
			return
		}
	} else if _, err := os.Stat(InFile); os.IsNotExist(err) {
		// If reading from a file, verify it exists.
		fmt.Println("Error: infile does not exist")
		flag.Usage()
		return
	}

	//////////////////////////////////////////////////////////////////////////
	// BLOCK : Create chans, buffers, and encoder for use
	//////////////////////////////////////////////////////////////////////////


	// create an opusEncoder to use
	if DCAMode == "decode" {
		OpusDecoder, err = gopus.NewDecoder(FrameRate, Channels)
		if err != nil {
			fmt.Println("NewDecoder Error: ", err)
			return
		}

		DecodeInputChan = make(chan []byte, 10)
		DecodeOutputChan = make(chan []int16, 10)
	} else {
		// create an opusEncoder to use
		OpusEncoder, err = gopus.NewEncoder(FrameRate, Channels, gopus.Audio)
		if err != nil {
			fmt.Println("NewEncoder Error: ", err)
			return
		}

		// set opus encoding options
		//	OpusEncoder.SetVbr(true)                // bool

		if Bitrate < 1 || Bitrate > 512 {
			Bitrate = 64 // Set to Discord default
		}
		OpusEncoder.SetBitrate(Bitrate * 1000)

		switch Application {
		case "voip":
			OpusEncoder.SetApplication(gopus.Voip)
		case "lowdelay":
			OpusEncoder.SetApplication(gopus.RestrictedLowDelay)
		default:
			OpusEncoder.SetApplication(gopus.Audio)
		}
		EncodeInputChan = make(chan []int16, 10)
		EncodeOutputChan = make(chan []byte, 10)

		if RawOutput == false {
			// Setup the metadata
			Metadata = MetadataStruct{
				Dca: &DCAMetadata{
					Version: FormatVersion,
					Tool: &DCAToolMetadata{
						Name:    "dca",
						Version: ProgramVersion,
						Url:     GitHubRepositoryURL,
						Author:  "bwmarrin",
					},
				},
				SongInfo: &SongMetadata{},
				Origin:   &OriginMetadata{},
				Opus: &OpusMetadata{
					Bitrate:     Bitrate * 1000,
					SampleRate:  FrameRate,
					Application: Application,
					FrameSize:   FrameSize,
					Channels:    Channels,
				},
				Extra: &ExtraMetadata{},
			}
			_ = Metadata

			// get ffprobe data
			if InFile == "pipe:0" {
				Metadata.Origin = &OriginMetadata{
					Source:   "pipe",
					Channels: Channels,
					Encoding: "pcm16/s16le",
				}
			} else if IsUrl {
				cmd := exec.Command("youtube-dl", "-i", "-j", "--youtube-skip-dash-manifest", InFile)
				cmd.Stderr = os.Stderr

				output, err := cmd.StdoutPipe()
				if err != nil {
					fmt.Println("StdoutPipe Error: ", err)
					return
				}

				err = cmd.Start()
				if err != nil {
					fmt.Println("RunStart Error: ", err)
					return
				}
				defer func() {
					cmd.Process.Kill()
					cmd.Wait()
				}()

				scanner := bufio.NewScanner(output)

				for scanner.Scan() {
					s := YTDLEntry{}
					err = json.Unmarshal(scanner.Bytes(), &s)
					if err != nil {
						fmt.Println(err)
						continue
					}

					Metadata.SongInfo = &SongMetadata{
						Title: s.Title,
					}

					Metadata.Origin = &OriginMetadata{
						Source:   "file",
						Encoding: s.Codec,
						Url:      s.Url,
					}
					break
				}
			} else {
				ffprobe := exec.Command("ffprobe", "-v", "quiet", "-print_format", "json", "-show_format", InFile)
				ffprobe.Stdout = &CmdBuf

				err = ffprobe.Start()
				if err != nil {
					fmt.Println("RunStart Error: ", err)
					return
				}

				err = ffprobe.Wait()
				if err != nil {
					fmt.Println("FFprobe Error: ", err)
					return
				}

				err = json.Unmarshal(CmdBuf.Bytes(), &FFprobeData)
				if err != nil {
					fmt.Println("Error unmarshaling the FFprobe JSON: ", err)
					return
				}

				bitrateInt, err := strconv.Atoi(FFprobeData.Format.Bitrate)
				if err != nil {
					fmt.Println("Could not convert bitrate to int: ", err)
					return
				}

				Metadata.SongInfo = &SongMetadata{
					Title:    FFprobeData.Format.Tags.Title,
					Artist:   FFprobeData.Format.Tags.Artist,
					Album:    FFprobeData.Format.Tags.Album,
					Genre:    FFprobeData.Format.Tags.Genre,
					Comments: "", // change later?
				}

				Metadata.Origin = &OriginMetadata{
					Source:   "file",
					Bitrate:  bitrateInt,
					Channels: Channels,
					Encoding: FFprobeData.Format.FormatLongName,
				}

				CmdBuf.Reset()

				// get cover art
				cover := exec.Command("ffmpeg", "-loglevel", "0", "-i", InFile, "-f", "singlejpeg", "pipe:1")
				cover.Stdout = &CmdBuf

				err = cover.Start()
				if err != nil {
					fmt.Println("RunStart Error: ", err)
					return
				}

				err = cover.Wait()
				if err == nil {
					buf := bytes.NewBufferString(CmdBuf.String())

					if CoverFormat == "png" {
						img, err := jpeg.Decode(buf)
						if err == nil { // silently drop it, no image
							err = png.Encode(&PngBuf, img)
							if err == nil {
								CoverImage = base64.StdEncoding.EncodeToString(PngBuf.Bytes())
							}
						}
					} else {
						CoverImage = base64.StdEncoding.EncodeToString(CmdBuf.Bytes())
					}

					Metadata.SongInfo.Cover = &CoverImage
				}

				CmdBuf.Reset()
				PngBuf.Reset()
			}
		}
	}

	//////////////////////////////////////////////////////////////////////////
	// BLOCK : Start reader and writer workers
	//////////////////////////////////////////////////////////////////////////

	if DCAMode == "decode" {
		wg.Add(1)
		go dcaReader()

		wg.Add(1)
		go decoder()

		wg.Add(1)
		go pcmWriter()
	} else {
		wg.Add(1)
		go pcmReader()

		wg.Add(1)
		go encoder()

		wg.Add(1)
		go dcaWriter()
	}

	// wait for above goroutines to finish, then exit.
	wg.Wait()
}

// dcaReader reads DCA from the input
func dcaReader() {

	defer func() {
		close(DecodeInputChan)
		wg.Done()
	}()

	var dcabuf io.Reader

	if InFile == "pipe:0" {
		dcabuf = bufio.NewReaderSize(os.Stdin, 16384)
	} else if IsUrl {
		resp, err := http.Get(InFile)
		if err != nil {
			fmt.Println("HTTP Request Error: ", err)
			return
		}

		dcabuf = resp.Body
	} else {
		file, err := os.Open(InFile)
		if err != nil {
			fmt.Println("Error opening file: ", err)
			return
		}

		dcabuf = bufio.NewReaderSize(file, 16384)
	}

	var dcaver uint32

	err = binary.Read(dcabuf, binary.LittleEndian, &dcaver)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return
	}
	if err != nil {
		fmt.Println("Error reading from dca: ", err)
		return
	}

	var jsonlen uint32

	err = binary.Read(dcabuf, binary.LittleEndian, &jsonlen)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return
	}
	if err != nil {
		fmt.Println("Error reading from dca: ", err)
		return
	}

	jsondata := make([]byte, jsonlen)

	err = binary.Read(dcabuf, binary.LittleEndian, &jsondata)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return
	}
	if err != nil {
		fmt.Println("Error reading from dca: ", err)
		return
	}

	var opuslen uint16
	for {
		err = binary.Read(dcabuf, binary.LittleEndian, &opuslen)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return
		}
		if err != nil {
			fmt.Println("Error reading from dca: ", err)
			return
		}

		// read opus data from dca
		opus := make([]byte, opuslen)
		err = binary.Read(dcabuf, binary.LittleEndian, &opus)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return
		}
		if err != nil {
			fmt.Println("Error reading from dca: ", err)
			return
		}

		// Send received opus to the decoder channel
		DecodeInputChan <- opus
	}
}

// pcmReader reads PCM from the input
func pcmReader() {

	defer func() {
		close(EncodeInputChan)
		wg.Done()
	}()

	// read from file
	if InFile == "pipe:0" {
		// read input from stdin pipe

		// 16KB input buffer
		rbuf := bufio.NewReaderSize(os.Stdin, 16384)
		for {

			// read data from stdin
			InBuf := make([]int16, FrameSize*Channels)

			err = binary.Read(rbuf, binary.LittleEndian, &InBuf)
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return
			}
			if err != nil {
				fmt.Println("Error reading from stdout: ", err)
				return
			}

			// write pcm data to the EncodeInputChan
			EncodeInputChan <- InBuf
		}
	} else if IsUrl {
		// Create a shell command "object" to run.
		ytdl := exec.Command("youtube-dl", "-v", "-f", "bestaudio", "-o", "-", InFile)
		ytdlout, err := ytdl.StdoutPipe()
		if err != nil {
			fmt.Println("ytdl StdoutPipe Error: ", err)
			return
		}
		ytdlbuf := bufio.NewReaderSize(ytdlout, 16384)

		// Create a shell command "object" to run.
		ffmpeg := exec.Command("ffmpeg", "-i", "pipe:0", "-vol", strconv.Itoa(Volume), "-f", "s16le", "-ar", strconv.Itoa(FrameRate), "-ac", strconv.Itoa(Channels), "pipe:1")
		ffmpeg.Stdin = ytdlbuf
		stdout, err := ffmpeg.StdoutPipe()
		if err != nil {
			fmt.Println("StdoutPipe Error: ", err)
			return
		}

		// Starts the youtube-dl command
		err = ytdl.Start()
		if err != nil {
			fmt.Println("RunStart Error: ", err)
			return
		}
		defer func() {
			go ytdl.Wait()
		}()

		// Starts the ffmpeg command
		err = ffmpeg.Start()
		if err != nil {
			fmt.Println("RunStart Error: ", err)
			return
		}

		for {

			// read data from ffmpeg stdout
			InBuf := make([]int16, FrameSize*Channels)
			err = binary.Read(stdout, binary.LittleEndian, &InBuf)
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return
			}
			if err != nil {
				fmt.Println("Error reading from ffmpeg stdout : ", err)
				return
			}

			// write pcm data to the EncodeInputChan
			EncodeInputChan <- InBuf

		}
	} else {
		// Create a shell command "object" to run.
		ffmpeg := exec.Command("ffmpeg", "-i", InFile, "-vol", strconv.Itoa(Volume), "-f", "s16le", "-ar", strconv.Itoa(FrameRate), "-ac", strconv.Itoa(Channels), "pipe:1")
		stdout, err := ffmpeg.StdoutPipe()
		if err != nil {
			fmt.Println("StdoutPipe Error: ", err)
			return
		}

		// Starts the ffmpeg command
		err = ffmpeg.Start()
		if err != nil {
			fmt.Println("RunStart Error: ", err)
			return
		}

		for {

			// read data from ffmpeg stdout
			InBuf := make([]int16, FrameSize*Channels)
			err = binary.Read(stdout, binary.LittleEndian, &InBuf)
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return
			}
			if err != nil {
				fmt.Println("Error reading from ffmpeg stdout : ", err)
				return
			}

			// write pcm data to the EncodeInputChan
			EncodeInputChan <- InBuf

		}
	}

}

// decoder listens on the DecodeInputChan and decodes provided opus data
// to PCM16, then sends the decoded data to the DecodeOutputChan
func decoder() {

	defer func() {
		close(DecodeOutputChan)
		wg.Done()
	}()

	for {
		opus, ok := <-DecodeInputChan
		if !ok {
			// if chan closed, exit
			return
		}

		// try decoding opus frame with Opus
		pcm, err := OpusDecoder.Decode(opus, FrameSize, false)
		if err != nil {
			fmt.Println("Decoding Error: ", err)
			return
		}

		// write pcm data to DecodeOutputChan
		DecodeOutputChan <- pcm
	}
}

// encoder listens on the EncodeInputChan and encodes provided PCM16 data
// to opus, then sends the encoded data to the EncodeOutputChan
func encoder() {

	defer func() {
		close(EncodeOutputChan)
		wg.Done()
	}()

	for {
		pcm, ok := <-EncodeInputChan
		if !ok {
			// if chan closed, exit
			return
		}

		// try encoding pcm frame with Opus
		opus, err := OpusEncoder.Encode(pcm, FrameSize, MaxBytes)
		if err != nil {
			fmt.Println("Encoding Error: ", err)
			return
		}

		// write opus data to EncodeOutputChan
		EncodeOutputChan <- opus
	}
}

// pcmWriter listens on the DecodeOutputChan and writes output to stdout
// or a file
func pcmWriter() {

	defer wg.Done()

	// 16KB output buffer
	var wbuf *bufio.Writer
	if OutFile == "pipe:1" {
		wbuf = bufio.NewWriterSize(os.Stdout, 16384)
	} else {
		file, err := os.Create(OutFile)
		if err != nil {
			fmt.Println("Failed to open output file: ", err)
			return
		}

		wbuf = bufio.NewWriterSize(file, 16384)
	}

	for {
		pcm, ok := <-DecodeOutputChan
		if !ok {
			// if chan closed, exit
			wbuf.Flush()
			return
		}

		// write pcm data to stdout
		err = binary.Write(wbuf, binary.LittleEndian, &pcm)
		if err != nil {
			fmt.Println("Error writing output: ", err)
			return
		}
	}
}

// dcaWriter listens on the EncodeOutputChan and writes output to stdout
// or a file
func dcaWriter() {

	defer wg.Done()

	var opuslen int16
	var jsonlen int32

	// 16KB output buffer
	var wbuf *bufio.Writer
	if OutFile == "pipe:1" {
		wbuf = bufio.NewWriterSize(os.Stdout, 16384)
	} else {
		file, err := os.Create(OutFile)
		if err != nil {
			fmt.Println("Failed to open output file: ", err)
			return
		}

		wbuf = bufio.NewWriterSize(file, 16384)
	}

	if RawOutput == false {
		// write the magic bytes
		wbuf.WriteString(MagicBytes)

		// encode and write json length
		json, err := json.Marshal(Metadata)
		if err != nil {
			fmt.Println("Failed to encode the Metadata JSON: ", err)
			return
		}

		jsonlen = int32(len(json))
		err = binary.Write(wbuf, binary.LittleEndian, &jsonlen)
		if err != nil {
			fmt.Println("Error writing output: ", err)
			return
		}

		// write the actual json
		wbuf.Write(json)
	}

	for {
		opus, ok := <-EncodeOutputChan
		if !ok {
			// if chan closed, exit
			wbuf.Flush()
			return
		}

		// write header
		opuslen = int16(len(opus))
		err = binary.Write(wbuf, binary.LittleEndian, &opuslen)
		if err != nil {
			fmt.Println("Error writing output: ", err)
			return
		}

		// write opus data to stdout
		err = binary.Write(wbuf, binary.LittleEndian, &opus)
		if err != nil {
			fmt.Println("Error writing output: ", err)
			return
		}
	}
}
