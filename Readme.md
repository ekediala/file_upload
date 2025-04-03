# Efficient Large Data Transfer Between Microservices (Go Demo)

This project provides a practical demonstration in Go for efficiently transferring large files or data payloads between microservices. It directly addresses the challenge posed by Sumit Mukhija in [this tweet](https://x.com/SumitM_X/status/1906687838609162530):

> "Your microservice needs to transfer large amounts of data (e.g., files, images) between services. How do you design the communication to avoid performance bottlenecks and manage large payloads efficiently?"

This implementation showcases a robust solution using standard HTTP protocols, focusing on:

*   **Memory Efficiency**: Avoids loading entire large files into memory on either the client or server side.
*   **Bandwidth Efficiency**: Uses selective compression to reduce network traffic for suitable content types.
*   **Resilience**: Implements resumable downloads to handle network interruptions gracefully.
*   **Performance**: Optimizes I/O using buffered readers/writers and standard library features.

## Key Features

*   **Chunked Transfers**: Uses HTTP `Range` requests to transfer data in manageable chunks (configurable size).
*   **Resumable Downloads**: The client can resume downloads from where they left off if interrupted.
*   **Selective Gzip Compression**: The server intelligently compresses *individual chunks* based on content type and chunk size, only when beneficial and supported by the client (`Accept-Encoding: gzip`).
*   **Memory Efficiency**: Leverages `io.Reader`, `io.Writer`, `io.LimitReader`, and `bufio` for streaming data without high memory allocation.
*   **Standard HTTP Protocols**: Relies entirely on standard HTTP/1.1 features (HEAD, GET, Range headers, Content-Encoding) for maximum interoperability.
*   **Graceful Shutdown**: Both client and server handle OS interrupt signals (SIGINT, SIGTERM, SIGHUP) for clean termination.
*   **Content Type Detection**: Server uses file extension and content sniffing (`http.DetectContentType`) to determine MIME types for compression decisions.

## Project Structure

```
file_upload/
├── Makefile           # Convenience script for running/building
├── go.mod             # Go module definition
├── cmd/
│   ├── client/
│   │   └── main.go    # Client service (acts as download trigger/proxy)
│   └── server/
│       └── main.go    # Server service (serves file chunks)
└── files/             # Directory containing files served by the server
    └── file.txt       # Sample file (you need to create this)
```

## How to Run

**Prerequisites:**

*   Go 1.23 or later installed.

**Steps:**

1.  **Clone the repository:**
    ```bash
    git clone https://github.com/ekediala/file_upload
    cd file_upload
    ```

2.  **Create the `files` directory:**
    ```bash
    mkdir files
    ```

3.  **Create a large test file:**
    This implementation shines with large files. Create one inside the `files` directory (e.g., a 100 MiB file):
    ```bash
    # On Linux/macOS:
    fallocate -l 100M files/large_test_file.txt
    # Or use dd:
    # dd if=/dev/zero of=files/large_test_file.txt bs=1M count=100
    ```
    *(Replace `large_test_file.txt` with your desired test filename)*

4.  **Start the services:**
    The `Makefile` provides a convenient way to start both services concurrently.
    ```bash
    make start
    ```
    This will run the server on port 8000 and the client on port 8888.

5.  **Trigger a download:**
    Open a new terminal and use `curl` (or another HTTP client) to request a download via the *client* service:
    ```bash
    curl http://localhost:8888/download/large_test_file.txt -o downloaded_file.txt
    ```
    *(Replace `large_test_file.txt` with the name of the file you created)*

6.  **Observe:**
    *   You will see logs in the `make start` terminal from both the client and server.
    *   The client logs will show the `Content-Range` header received for each chunk.
    *   The client will print the total download time and size upon completion.
    *   The file `downloaded_file.txt` will be created in the directory where you ran `curl`.

7.  **Stop the services:**
    Press `Ctrl+C` in the terminal where `make start` is running. Both services should shut down gracefully.

## How it Works

### Server (`cmd/server/main.go`)

*   Listens on port 8000.
*   Serves files from the `./files` directory relative to its execution path.
*   **`HEAD /download/{fileName}`:**
    *   Checks file existence and stats.
    *   Sets the `Content-Length` header to the total file size.
    *   Returns `200 OK` without a body.
*   **`GET /download/{fileName}`:**
    *   Requires a `Range` header (e.g., `bytes=0-524287`).
    *   Validates the requested range against the file size.
    *   Determines the `Content-Type` using `getContentType` (extension + sniffing).
    *   Checks if the client sent `Accept-Encoding: gzip`.
    *   Decides whether to **compress the requested chunk** based on:
        *   Client support (`Accept-Encoding`).
        *   `isCompressibleType()` check (text-based formats).
        *   Chunk size (`>= 8KB` threshold).
    *   Seeks to the `start` byte of the requested range in the file.
    *   Creates an `io.LimitReader` to read *only* the `chunkSize` bytes.
    *   **If compressing:**
        *   Sets `Content-Encoding: gzip`.
        *   Sets `Content-Range`.
        *   Writes `206 Partial Content`.
        *   Wraps the response writer with `gzip.NewWriterLevel(w, gzip.BestSpeed)`.
        *   `io.Copy`s the `chunkReader` data through the gzip writer.
    *   **If not compressing:**
        *   Sets `Content-Length` to the `chunkSize`.
        *   Sets `Content-Range`.
        *   Writes `206 Partial Content`.
        *   `io.Copy`s the `chunkReader` data directly to the response writer.

### Client (`cmd/client/main.go`)

*   Listens on port 8888. Acts as a download trigger/proxy.
*   **`GET /download/{fileName}`:**
    *   Receives the request for a specific file.
    *   Opens/Creates the local file (`fileName`) for writing.
    *   Performs a `HEAD` request to the server (`serviceUrl`, port 8000) to get the `totalSize`.
    *   Checks the local file size (`fileSize`) using `os.Stat`.
    *   If `fileSize >= totalSize`, the download is already complete.
    *   If partial, `file.Seek`s to `fileSize` to resume writing.
    *   Initializes a `bufio.NewWriterSize` for efficient disk I/O.
    *   **Enters a loop:** (`for start := fileSize; start < totalSize; start += chunkSize`)
        *   Calculates `start` and `end` for the next chunk (default 512KiB).
        *   Calls `downloadChunk` to fetch the specific chunk.
    *   Logs total download time and size.
    *   Responds `Download complete` to the original `curl` request.
*   **`downloadChunk` function:**
    *   Creates a `GET` request to the server (`serviceUrl`).
    *   Sets the `Range` header (e.g., `bytes=0-524287`).
    *   Sets the `Accept-Encoding: gzip` header to signal compression support.
    *   Sends the request using the default HTTP client.
    *   Checks the response `Content-Encoding` header. If `gzip`, wraps the response body with `gzip.NewReader`.
    *   Handles potential HTTP error status codes (`>= 400`).
    *   `io.Copy`s data from the (potentially decompressed) `reader` to the `bufio.Writer` (which writes to the local file).
    *   Logs the `Content-Range` header received from the server.
    *   Returns the HTTP status code (`206 Partial Content` expected).

## Configuration

Key parameters are defined as constants:

*   `cmd/server/main.go`:
    *   `Port`: 8000 (Server listening port)
    *   `ContentFolderName`: "files" (Directory to serve files from)
*   `cmd/client/main.go`:
    *   `serviceUrl`: "http://localhost:8000" (URL of the server)
    *   `chunkSize`: 512 * 1024 (512 KiB - Size of chunks requested by the client)
    *   `bufferSize`: 64 * 1024 (64 KiB - `bufio.Writer` buffer size for disk writes)
    *   `Port`: 8888 (Client service listening port)
    *   `Mib`: 1_000_000 (Used for rough MiB calculation in logging)

*Server Compression Logic:*
*   Chunks smaller than 8KB (`8*1024`) are not compressed.
*   Only content types deemed compressible by `isCompressibleType` are compressed.