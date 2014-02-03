package utils

import (
	"compress/bzip2"
	"compress/gzip"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
)

// Downloader is parallel HTTP fetcher
type Downloader interface {
	Download(url string, destination string, result chan<- error)
	DownloadWithChecksum(url string, destination string, result chan<- error, expected ChecksumInfo)
	Pause()
	Resume()
	Shutdown()
}

// Check interface
var (
	_ Downloader = &downloaderImpl{}
)

// downloaderImpl is implementation of Downloader interface
type downloaderImpl struct {
	queue   chan *downloadTask
	stop    chan bool
	stopped chan bool
	pause   chan bool
	unpause chan bool
	threads int
}

// downloadTask represents single item in queue
type downloadTask struct {
	url         string
	destination string
	result      chan<- error
	expected    ChecksumInfo
}

// NewDownloader creates new instance of Downloader which specified number
// of threads
func NewDownloader(threads int) Downloader {
	downloader := &downloaderImpl{
		queue:   make(chan *downloadTask, 1000),
		stop:    make(chan bool),
		stopped: make(chan bool),
		pause:   make(chan bool),
		unpause: make(chan bool),
		threads: threads,
	}

	for i := 0; i < downloader.threads; i++ {
		go downloader.process()
	}

	return downloader
}

// Shutdown stops downloader after current tasks are finished,
// but doesn't process rest of queue
func (downloader *downloaderImpl) Shutdown() {
	for i := 0; i < downloader.threads; i++ {
		downloader.stop <- true
	}

	for i := 0; i < downloader.threads; i++ {
		<-downloader.stopped
	}
}

// Pause pauses task processing
func (downloader *downloaderImpl) Pause() {
	for i := 0; i < downloader.threads; i++ {
		downloader.pause <- true
	}
}

// Resume resumes task processing
func (downloader *downloaderImpl) Resume() {
	for i := 0; i < downloader.threads; i++ {
		downloader.unpause <- true
	}
}

// Download starts new download task
func (downloader *downloaderImpl) Download(url string, destination string, result chan<- error) {
	downloader.DownloadWithChecksum(url, destination, result, ChecksumInfo{Size: -1})
}

// DownloadWithChecksum starts new download task with checksum verification
func (downloader *downloaderImpl) DownloadWithChecksum(url string, destination string, result chan<- error, expected ChecksumInfo) {
	downloader.queue <- &downloadTask{url: url, destination: destination, result: result, expected: expected}
}

// handleTask processes single download task
func (downloader *downloaderImpl) handleTask(task *downloadTask) {
	fmt.Printf("Downloading %s...\n", task.url)

	resp, err := http.Get(task.url)
	if err != nil {
		task.result <- err
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		task.result <- fmt.Errorf("HTTP code %d while fetching %s", resp.StatusCode, task.url)
		return
	}

	err = os.MkdirAll(filepath.Dir(task.destination), 0755)
	if err != nil {
		task.result <- err
		return
	}

	temppath := task.destination + ".down"

	outfile, err := os.Create(temppath)
	if err != nil {
		task.result <- err
		return
	}
	defer outfile.Close()

	var w io.Writer

	checksummer := NewChecksumWriter()

	if task.expected.Size != -1 {
		w = io.MultiWriter(outfile, checksummer)
	} else {
		w = outfile
	}

	_, err = io.Copy(w, resp.Body)
	if err != nil {
		os.Remove(temppath)
		task.result <- err
		return
	}

	if task.expected.Size != -1 {
		actual := checksummer.Sum()

		if actual.Size != task.expected.Size {
			err = fmt.Errorf("%s: size check mismatch %d != %d", task.url, actual.Size, task.expected.Size)
		} else if task.expected.MD5 != "" && actual.MD5 != task.expected.MD5 {
			err = fmt.Errorf("%s: md5 hash mismatch %#v != %#v", task.url, actual.MD5, task.expected.MD5)
		} else if task.expected.SHA1 != "" && actual.SHA1 != task.expected.SHA1 {
			err = fmt.Errorf("%s: sha1 hash mismatch %#v != %#v", task.url, actual.SHA1, task.expected.SHA1)
		} else if task.expected.SHA256 != "" && actual.SHA256 != task.expected.SHA256 {
			err = fmt.Errorf("%s: sha256 hash mismatch %#v != %#v", task.url, actual.SHA256, task.expected.SHA256)
		}

		if err != nil {
			os.Remove(temppath)
			task.result <- err
			return
		}
	}

	err = os.Rename(temppath, task.destination)
	if err != nil {
		os.Remove(temppath)
		task.result <- err
		return
	}

	task.result <- nil
}

// process implements download thread in goroutine
func (downloader *downloaderImpl) process() {
	for {
		select {
		case <-downloader.stop:
			downloader.stopped <- true
			return
		case <-downloader.pause:
			<-downloader.unpause
		case task := <-downloader.queue:
			downloader.handleTask(task)
		}
	}
}

// DownloadTemp starts new download to temporary file and returns File
//
// Temporary file would be already removed, so no need to cleanup
func DownloadTemp(downloader Downloader, url string) (*os.File, error) {
	tempdir, err := ioutil.TempDir(os.TempDir(), "aptly")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tempdir)

	tempfile := filepath.Join(tempdir, "buffer")

	ch := make(chan error, 1)
	downloader.Download(url, tempfile, ch)

	err = <-ch
	if err != nil {
		return nil, err
	}

	file, err := os.Open(tempfile)
	if err != nil {
		return nil, err
	}

	return file, nil
}

// List of extensions + corresponding uncompression support
var compressionMethods = []struct {
	extenstion     string
	transformation func(io.Reader) (io.Reader, error)
}{
	{
		extenstion:     ".bz2",
		transformation: func(r io.Reader) (io.Reader, error) { return bzip2.NewReader(r), nil },
	},
	{
		extenstion:     ".gz",
		transformation: func(r io.Reader) (io.Reader, error) { return gzip.NewReader(r) },
	},
	{
		extenstion:     "",
		transformation: func(r io.Reader) (io.Reader, error) { return r, nil },
	},
}

// DownloadTryCompression tries to download from URL .bz2, .gz and raw extension until
// it finds existing file.
func DownloadTryCompression(downloader Downloader, url string) (io.Reader, *os.File, error) {
	var err error

	for _, method := range compressionMethods {
		var file *os.File

		file, err = DownloadTemp(downloader, url+method.extenstion)
		if err != nil {
			continue
		}

		var uncompressed io.Reader
		uncompressed, err = method.transformation(file)
		if err != nil {
			continue
		}

		return uncompressed, file, err
	}
	return nil, nil, err
}
