package baichuan

import (
	"context"
	"encoding/xml"
	"fmt"
	"time"
)

// Recordings on a camera's SD card. The requests are the documents the
// Reolink client sends (captured 2026-10-22 against an Argus PT Ultra, see
// docs/sd-ptz-audio.md), each sent as one PayloadOff=0 document:
//
//	142 DayRecords        which days in a range have recordings
//	14 / 15 / 16          find files in a day, next batch, close the search
//	143                   download a time range as media packets

const (
	msgDayRecords = 142
	msgFindFile   = 14
	msgFindNext   = 15
	msgFindClose  = 16
	msgDownload   = 143
)

// RecordFile is one recording on the card.
type RecordFile struct {
	Name       string
	Path       string
	Type       string
	Start, End time.Time
	Size       int64
	HasAudio   bool
	HasSub     bool
}

type sdTime struct {
	Year   int `xml:"year"`
	Month  int `xml:"month"`
	Day    int `xml:"day"`
	Hour   int `xml:"hour"`
	Minute int `xml:"minute"`
	Second int `xml:"second"`
}

func (t sdTime) time() time.Time {
	return time.Date(t.Year, time.Month(t.Month), t.Day, t.Hour, t.Minute, t.Second, 0, time.Local)
}

func sdTimeXML(tag string, t time.Time) string {
	return fmt.Sprintf("<%s>\n<year>%d</year>\n<month>%d</month>\n<day>%d</day>\n<hour>%d</hour>\n<minute>%d</minute>\n<second>%d</second>\n</%s>\n",
		tag, t.Year(), int(t.Month()), t.Day(), t.Hour(), t.Minute(), t.Second(), tag)
}

const xmlHead = "<?xml version=\"1.0\" encoding=\"UTF-8\" ?>\n<body>\n"

func dayRecordsDoc(channel int, from, to time.Time) []byte {
	return []byte(xmlHead + "<DayRecords version=\"1.1\">\n" + sdTimeXML("startTime", from) + sdTimeXML("endTime", to) +
		fmt.Sprintf("<DayRecordList>\n<DayRecord>\n<index>0</index>\n<channelId>%d</channelId>\n</DayRecord>\n</DayRecordList>\n</DayRecords>\n</body>\n", channel))
}

// parseDayRecords returns the days that have recordings, as dates: the
// reply counts days from the request's start day.
func parseDayRecords(doc []byte, from time.Time) ([]time.Time, error) {
	var d struct {
		Days []struct {
			Index int    `xml:"index"`
			Type  string `xml:"type"`
		} `xml:"DayRecords>DayRecordList>DayRecord>dayTypeList>dayType"`
	}
	if err := xml.Unmarshal(doc, &d); err != nil {
		return nil, fmt.Errorf("baichuan: day records: %w", err)
	}
	base := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, from.Location())
	var out []time.Time
	for _, x := range d.Days {
		out = append(out, base.AddDate(0, 0, x.Index))
	}
	return out, nil
}

func findFileDoc(channel int, stream string, from, to time.Time) []byte {
	return []byte(xmlHead + "<FileInfoList version=\"1.1\">\n<FileInfo>\n<searchAITrack>1</searchAITrack>\n" +
		fmt.Sprintf("<channelId>%d</channelId>\n<logicChnBitmap>255</logicChnBitmap>\n<streamType>%s</streamType>\n", channel, stream) +
		"<recordType>manual, sched, io, md, people, face, vehicle, dog_cat, visitor, other, package</recordType>\n" +
		sdTimeXML("startTime", from) + sdTimeXML("endTime", to) + "</FileInfo>\n</FileInfoList>\n</body>\n")
}

func handleDoc(channel, handle int) []byte {
	return []byte(xmlHead + fmt.Sprintf("<FileInfoList version=\"1.1\">\n<FileInfo>\n<channelId>%d</channelId>\n<searchAITrack>1</searchAITrack>\n<handle>%d</handle>\n</FileInfo>\n</FileInfoList>\n</body>\n", channel, handle))
}

type fileInfoXML struct {
	Handle     int    `xml:"handle"`
	Name       string `xml:"name"`
	ID         string `xml:"Id"`
	RecordType string `xml:"recordType"`
	SizeL      int64  `xml:"sizeL"`
	SizeH      int64  `xml:"sizeH"`
	HasAudio   int    `xml:"containsAudio"`
	SupportSub int    `xml:"supportSub"`
	Start      sdTime `xml:"startTime"`
	End        sdTime `xml:"endTime"`
}

// parseFiles reads a find or next reply: the search handle and the files
// it carries (none once the search is exhausted).
func parseFiles(doc []byte) (handle int, files []RecordFile, err error) {
	var d struct {
		Items []fileInfoXML `xml:"FileInfoList>FileInfo"`
	}
	if err := xml.Unmarshal(doc, &d); err != nil {
		return 0, nil, fmt.Errorf("baichuan: file list: %w", err)
	}
	for _, it := range d.Items {
		if it.Handle != 0 {
			handle = it.Handle
		}
		if it.ID == "" && it.Name == "" {
			continue
		}
		files = append(files, RecordFile{
			Name: it.Name, Path: it.ID, Type: it.RecordType,
			Start: it.Start.time(), End: it.End.time(),
			Size: it.SizeH<<32 | it.SizeL, HasAudio: it.HasAudio == 1, HasSub: it.SupportSub == 1,
		})
	}
	return handle, files, nil
}

// request sends one SD document and waits for the reply to it.
func (c *Conn) sdRequest(ctx context.Context, id uint32, doc []byte) ([]byte, error) {
	if err := c.SendDocument(id, doc); err != nil {
		return nil, err
	}
	m, err := awaitMessage(ctx, c, id)
	if err != nil {
		return nil, err
	}
	if s := m.Header.Status(); s != 200 && s != 0 {
		return nil, fmt.Errorf("baichuan: message %d: camera answered status %d", id, s)
	}
	return m.XML, nil
}

// RecordDays lists the days in month that have recordings. One month per
// request, as the client asks: a range spanning two months comes back with
// no days at all (seen on an Argus PT Ultra).
func (c *Conn) RecordDays(ctx context.Context, month time.Time) ([]time.Time, error) {
	from := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, month.Location())
	to := from.AddDate(0, 1, 0).Add(-time.Second)
	doc, err := c.sdRequest(ctx, msgDayRecords, dayRecordsDoc(c.opts.Channel, from, to))
	if err != nil {
		return nil, err
	}
	return parseDayRecords(doc, from)
}

// RecordFiles lists the recordings that start on day, oldest first.
func (c *Conn) RecordFiles(ctx context.Context, day time.Time) ([]RecordFile, error) {
	from := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, day.Location())
	to := from.Add(24*time.Hour - time.Second)
	doc, err := c.sdRequest(ctx, msgFindFile, findFileDoc(c.opts.Channel, "mainStream", from, to))
	if err != nil {
		return nil, err
	}
	handle, files, err := parseFiles(doc)
	if err != nil {
		return nil, err
	}
	if handle == 0 {
		return files, nil
	}
	defer c.sdRequest(ctx, msgFindClose, handleDoc(c.opts.Channel, handle))
	for i := 0; i < 200; i++ {
		doc, err := c.sdRequest(ctx, msgFindNext, handleDoc(c.opts.Channel, handle))
		if err != nil {
			return files, err
		}
		_, more, err := parseFiles(doc)
		if err != nil {
			return files, err
		}
		if len(more) == 0 {
			break
		}
		files = append(files, more...)
	}
	return files, nil
}

func downloadDoc(channel int, sub bool, from, to time.Time) []byte {
	stream, supportSub := "mainStream", 0
	if sub {
		stream, supportSub = "subStream", 1
	}
	return []byte(xmlHead + "<FileInfoList version=\"1.1\">\n<FileInfo>\n<logicChnBitmap>255</logicChnBitmap>\n" +
		fmt.Sprintf("<channelId>%d</channelId>\n<supportSub>%d</supportSub>\n<streamType>%s</streamType>\n", channel, supportSub, stream) +
		sdTimeXML("startTime", from) + sdTimeXML("endTime", to) + "</FileInfo>\n</FileInfoList>\n</body>\n")
}

// errDownloadDone and errDownloadQuiet are the two ways a download that
// delivered data ends; Download reports which as end, with a nil error.
var (
	errDownloadDone  = fmt.Errorf("camera marked the end")
	errDownloadQuiet = fmt.Errorf("camera went quiet")
)

// Download asks the camera for the recording between from and to and
// passes every media frame to fn as it arrives. The camera starts at the
// requested second, not at a keyframe, so frames before the first I frame
// are dropped. It returns when the camera has been quiet for idle, when fn
// returns an error, or when ctx ends.
func (c *Conn) Download(ctx context.Context, from, to time.Time, sub bool, idle time.Duration, fn func(Frame) error) (end string, err error) {
	err = c.download(ctx, from, to, sub, idle, fn)
	if err == errDownloadDone || err == errDownloadQuiet {
		return err.Error(), nil
	}
	return "", err
}

func (c *Conn) download(ctx context.Context, from, to time.Time, sub bool, idle time.Duration, fn func(Frame) error) error {
	if err := c.SendDocument(msgDownload, downloadDoc(c.opts.Channel, sub, from, to)); err != nil {
		return err
	}
	dp := NewDepacketizer()
	started, sawKey := false, false
	quiet := time.NewTimer(30 * time.Second) // first data can take a while on a sleeping camera
	defer quiet.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-quiet.C:
			if !started {
				return fmt.Errorf("baichuan: download: the camera sent nothing")
			}
			return errDownloadQuiet
		case m, ok := <-c.Messages():
			if !ok {
				if started {
					return nil
				}
				return fmt.Errorf("baichuan: download: connection closed")
			}
			if m.Header.MsgID != msgDownload {
				continue
			}
			if s := m.Header.Status(); len(m.Payload) == 0 && s != 200 && s != 0 {
				// 300 after the data is the camera saying the range is done.
				if s == 300 && started {
					return errDownloadDone
				}
				return fmt.Errorf("baichuan: download: camera answered status %d", s)
			}
			if len(m.Payload) == 0 {
				continue
			}
			started = true
			quiet.Reset(idle)
			// A recording arrives as one byte stream cut into messages
			// wherever the camera's buffer filled, not at packet starts,
			// so a message boundary must not reset the depacketizer: it
			// threw away every keyframe split across two messages and a
			// TrackMix recording came back with 0 frames (#21).
			dp.Write(m.Payload, false)
			for {
				f, ok := dp.Next()
				if !ok {
					break
				}
				if f.Kind == FrameIFrame {
					sawKey = true
				}
				if !sawKey || f.Kind == FrameInfo {
					continue
				}
				if err := fn(f); err != nil {
					return err
				}
			}
		}
	}
}
