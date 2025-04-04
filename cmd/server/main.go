package main

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"
)

const (
	Port               = 8000
	ContentFolderName  = "files"
	MinCompressionSize = 8 * 1024 // 8kb
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

	// we have one handler handle both the HEAD and GET requests
	// we want to maintain the coupling between both requests
	mux.HandleFunc("HEAD /download/{fileName}", Handler)
	mux.HandleFunc("GET /download/{fileName}", Handler)

	server := http.Server{
		Handler: mux,
		Addr:    fmt.Sprintf(":%d", Port),
	}

	fmt.Println("Started server on port:", Port)

	// start the server in a goroutine. We could just start the server on the main
	// thread but that introduces problems for our clean up process. If we listen to
	// the clean up in a goroutine we won't be able to wait for the shutdown process
	// to finish before exiting main.
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

func Handler(w http.ResponseWriter, r *http.Request) {
	fileName := r.PathValue("fileName")
	// ensure this request is not trying to do something fishy
	if strings.Contains(fileName, "..") {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	dir, err := os.Getwd()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	fileName = path.Join(dir, ContentFolderName, fileName)
	file, err := os.Open(fileName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	stat, err := file.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// if it is a head request, we send back the file size. The client will use
	// that data for resumability i.e to tell us if parts of the file have already
	// been downloaded and therefore where to resume from. The client can also use
	// the size to decide how to chunk the data to achieve a balance between number
	// of http calls and download speed per chunk.
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", stat.Size()))
		return
	}

	// Parse range header (required for our implementation)
	rangeHeader := r.Header.Get("Range")
	if rangeHeader == "" {
		http.Error(w, "Range header required", http.StatusBadRequest)
		return
	}

	// Parse the range
	var start, end int64
	n, err := fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end)
	if err != nil || n != 2 {
		http.Error(w, "Invalid range format", http.StatusBadRequest)
		return
	}

	// Validate range
	if start < 0 || end < start || end >= stat.Size() {
		http.Error(w, "Invalid range", http.StatusRequestedRangeNotSatisfiable)
		return
	}

	chunkSize := end - start + 1
	contentType := getContentType(fileName, file)
	w.Header().Set("Content-Type", contentType)

	// Set the file offset to the provided start point.
	// We do not want to read from the start of the file.
	// We want to "resume" from where they stopped
	_, err = file.Seek(start, io.SeekStart)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Create a limited reader for just this chunk.
	// Why? This is exactly why it exists. Alternatively, we could create a
	// buffer to minimize syscalls but it won't be necessary here.
	// We use io.copy to stream the response to the client. Given we also don't
	// know what is contained in the file, we will have to read the whole chunk size
	// into memory:
	//
	// data := make([]byte, chunkSize)
	// reader := bufio.NewReader(file)
	// n, _ := reader.Read(data)
	//
	// we have already added 516kb of memory to the program [512 for chunk size
	// and 4kb for the buffer] and lost the benefits of streaming. Just 10 concurrent
	// requests and we are already at 5mb of memory.
	//
	// Meanwhile we already have io.Copy with its buffer that streams 32kb chunks from the file to the connection.
	reader := io.LimitReader(file, chunkSize)
	
	// Check if we should compress this chunk.
	// We only want to compress when it is beneficial
	acceptsGzip := strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
	shouldCompress := acceptsGzip &&
		isCompressibleType(contentType) &&
		chunkSize >= MinCompressionSize // Only compress chunks >= 8KB

	if shouldCompress {
		// For compressed chunks
		w.Header().Set("Content-Encoding", "gzip")
		// Cannot predict final Content-Length after compression
		// Set partial content status
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, stat.Size()))
		w.WriteHeader(http.StatusPartialContent)

		// Create gzip writer with fast compression
		gz, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer gz.Close()

		// Send compressed chunk
		_, err = io.Copy(gz, reader)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		return
	}

	// For uncompressed chunks
	w.Header().Set("Content-Length", fmt.Sprintf("%d", chunkSize))
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, stat.Size()))
	w.WriteHeader(http.StatusPartialContent)

	// Send uncompressed chunk
	_, err = io.Copy(w, reader)
	if err != nil {
		log.Printf("Error sending chunk: %v", err)
		return
	}
}

func isCompressibleType(contentType string) bool {
	compressibleTypes := []string{
		"text/", "application/json", "application/xml",
		"application/javascript", "application/x-javascript",
	}

	for _, t := range compressibleTypes {
		if strings.Contains(contentType, t) {
			return true
		}
	}
	return false
}

// getContentType determines the content type of a file using both
// extension-based matching and content sniffing when necessary.
func getContentType(fileName string, fileReader io.ReadSeeker) string {
	// 1. Try to determine content type from file extension
	ext := strings.ToLower(path.Ext(fileName))
	if ext != "" {
		mimeType := mime.TypeByExtension(ext)
		if mimeType != "" {
			if idx := strings.Index(mimeType, ";"); idx != -1 {
				return mimeType[:idx]
			}
			return mimeType
		}
	}

	// 2. For common extensions not in standard library
	switch ext {
	case ".md":
		return "text/markdown"
	case ".jsx", ".tsx":
		return "application/javascript"
	case ".yaml", ".yml":
		return "application/yaml"
		// ... other custom mappings
	}

	// 3. If we have a file reader, try content sniffing
	if fileReader != nil {
		// Save current position
		currentPos, err := fileReader.Seek(0, io.SeekCurrent)
		if err == nil {
			// Read first 512 bytes for content detection
			buffer := make([]byte, 512)
			n, err := fileReader.Read(buffer)

			// Restore original position
			fileReader.Seek(currentPos, io.SeekStart)

			if err == nil {
				// Detect content type
				return http.DetectContentType(buffer[:n])
			}
		}
	}

	// 4. Fallback to binary
	return "application/octet-stream"
}
