package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	serviceUrl = "http://localhost:8000"
	chunkSize  = 512 * 1024
	bufferSize = 64 * 1024
	Port       = 8888
	Mib        = 1_000_000
)

var signals = []os.Signal{
	syscall.SIGINT,  // Ctrl+C
	syscall.SIGTERM, // Termination request
	syscall.SIGHUP,  // Terminal closed
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), signals...)
	defer cancel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /download/{fileName}", FileDownloadHandler)

	server := http.Server{
		Handler: mux,
		Addr:    fmt.Sprintf(":%d", Port),
	}

	fmt.Println("Started server on port:", Port)

	go func() {
		err := server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			return
		}
		log.Fatal(err)
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	err := server.Shutdown(shutdownCtx)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}

	fmt.Println("Server shutdown successfully.")
}

func FileDownloadHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	fileName := r.PathValue("fileName")
	if strings.Contains(fileName, "..") {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	file, err := os.OpenFile(fileName, os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer file.Close()

	// Get file info to check existing size
	stat, err := file.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	fileSize := stat.Size()
	client := http.DefaultClient
	url := fmt.Sprintf("%s/download/%s", serviceUrl, fileName)

	// make head request to get the file size. this helps with resumability
	req, err := http.NewRequestWithContext(r.Context(), http.MethodHead, url, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	res, err := client.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		var b bytes.Buffer
		_, err := io.Copy(&b, res.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		http.Error(w, b.String(), http.StatusInternalServerError)
		return
	}

	totalSize := res.ContentLength
	if fileSize >= totalSize {
		w.Write([]byte("File already downloaded"))
		return
	}

	_, err = file.Seek(fileSize, io.SeekStart)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writer := bufio.NewWriterSize(file, bufferSize)
	defer writer.Flush()

	for start := fileSize; start < totalSize; start += chunkSize {
		end := start + chunkSize - 1
		if end > totalSize {
			end = totalSize - 1
		}

		statusCode, err := downloadChunk(r.Context(), client, writer, url, start, end)
		if err != nil {
			http.Error(w, err.Error(), statusCode)
			return
		}
	}

	fmt.Printf("Took %fs to download %dmib\n", time.Since(start).Seconds(), totalSize/Mib)
	w.Write([]byte("Download complete"))
}

func downloadChunk(ctx context.Context, client *http.Client, w io.Writer, url string, start, end int64) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return http.StatusInternalServerError, err
	}

	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	req.Header.Set("Accept-Encoding", "gzip")

	res, err := client.Do(req)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	defer res.Body.Close()

	var reader io.Reader = res.Body

	if res.Header.Get("Content-Encoding") == "gzip" {
		gzipReader, err := gzip.NewReader(res.Body)
		if err != nil {
			return http.StatusInternalServerError, err
		}
		defer gzipReader.Close()
		reader = gzipReader
	}

	if res.StatusCode >= http.StatusBadRequest {
		var b bytes.Buffer
		_, err := io.Copy(&b, reader)
		if err != nil {
			return http.StatusInternalServerError, err
		}

		return res.StatusCode, fmt.Errorf(b.String())
	}

	_, err = io.Copy(w, reader)
	if err != nil {
		return http.StatusInternalServerError, err
	}

	fmt.Printf("Downloaded %s\n", res.Header.Get("Content-Range"))

	return res.StatusCode, nil
}
