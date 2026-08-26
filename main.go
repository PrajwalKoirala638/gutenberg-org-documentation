package main // this file belongs to the main package, the entry point of the program

import (
	"context"   // context lets us cancel in-flight requests when the user hits ctrl+c
	"fmt"       // fmt lets us format strings like the url we build
	"io"        // io lets us read the full response body into memory
	"log"       // log lets us print messages with timestamps and control output better than fmt
	"net/http"  // net/http lets us make http requests like visiting a webpage
	"os"        // os lets us access the ctrl+c interrupt signal from the operating system
	"os/signal" // os/signal lets us listen for that interrupt signal
	"strings"   // strings lets us check if the page content contains a specific phrase
	"time"      // time lets us set timeouts, delays, and backoff durations
)

// fetchResult bundles everything visitEbookPageAndGetContent needs to report back to the caller.
type fetchResult struct { // this struct groups related return values together instead of returning many separate values
	content    string // the raw page content that was fetched
	statusCode int    // the http status code the server responded with
	notFound   bool   // true if either the status code or the page text indicates this ebook does not exist
}

// visitEbookPageAndGetContent visits the Gutenberg ebook page for the given ebookNumber
// and returns the page content as a string, using the shared httpClient passed in.
func visitEbookPageAndGetContent(ctx context.Context, httpClient *http.Client, ebookNumber int, userAgent string, notFoundPhrase string) (fetchResult, error) { // every value it needs comes in as a parameter, nothing global
	pageUrl := fmt.Sprintf("https://www.gutenberg.org/ebooks/%d", ebookNumber) // build the full url by inserting the ebook number

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, pageUrl, nil) // build a request tied to our context so it can be cancelled on ctrl+c
	if err != nil {                                                               // check if building the request itself failed
		return fetchResult{}, err // return an empty result and the error
	}
	request.Header.Set("User-Agent", userAgent) // attach our user agent header so the server knows what is visiting it

	response, err := httpClient.Do(request) // send the request using our shared client
	if err != nil {                         // check if the request failed (timeout, cancellation, network error, etc)
		return fetchResult{}, err // return an empty result and the error so the caller can decide whether to retry
	}
	defer response.Body.Close() // make sure the response body is closed once we are done reading it

	bodyBytes, err := io.ReadAll(response.Body) // read the entire response body into a byte slice
	if err != nil {                             // check if reading the body failed
		return fetchResult{}, err // return an empty result and the error
	}

	pageContent := string(bodyBytes) // convert the raw bytes into a readable string

	isNotFoundStatus := response.StatusCode == http.StatusNotFound  // true if the server responded with a plain 404
	isNotFoundText := strings.Contains(pageContent, notFoundPhrase) // true if the page body contains the "not found" phrase

	result := fetchResult{ // build the result struct to hand back to the caller
		content:    pageContent,                        // the page text we fetched
		statusCode: response.StatusCode,                // the status code the server returned
		notFound:   isNotFoundStatus || isNotFoundText, // either signal is enough to consider this "not found"
	}

	return result, nil // return the result and no error since everything succeeded
}

// fetchWithRetries wraps visitEbookPageAndGetContent with retry-and-backoff logic,
// so a single timeout or network hiccup does not immediately give up on an index.
func fetchWithRetries(ctx context.Context, httpClient *http.Client, ebookNumber int, userAgent string, notFoundPhrase string, maxRetries int, backoffBase time.Duration) (fetchResult, error) { // every tunable value comes in as a parameter
	var lastErr error // this variable remembers the most recent error across attempts

	for attempt := 1; attempt <= maxRetries; attempt++ { // try up to maxRetries times
		result, err := visitEbookPageAndGetContent(ctx, httpClient, ebookNumber, userAgent, notFoundPhrase) // attempt to fetch the page
		if err == nil {                                                                                     // check if this attempt succeeded
			return result, nil // return the successful result immediately, no need to retry further
		}

		lastErr = err // remember this error in case all attempts fail

		if ctx.Err() != nil { // check if the context was cancelled (e.g. ctrl+c) during this attempt
			return fetchResult{}, ctx.Err() // stop retrying immediately and report the cancellation
		}

		backoffDuration := time.Duration(attempt) * backoffBase                                                                  // grow the wait time with each attempt (2s, 4s, 6s...)
		log.Printf("index %d: attempt %d/%d failed: %v, retrying in %v", ebookNumber, attempt, maxRetries, err, backoffDuration) // log the failure and upcoming retry

		select { // wait for either the backoff timer or a cancellation, whichever comes first
		case <-time.After(backoffDuration): // the backoff period passed normally
			// continue to the next attempt
		case <-ctx.Done(): // the context was cancelled while we were waiting
			return fetchResult{}, ctx.Err() // stop retrying and report the cancellation
		}
	}

	return fetchResult{}, lastErr // all attempts failed, return the last error we saw
}

func main() { // program execution starts here
	requestTimeout := 3 * time.Minute // local variable: how long we wait before giving up on a single request

	userAgent := "Mozilla/5.0 (compatible; GutenbergCrawler/1.0; +https://example.com/bot)" // local variable: identifies our program to the server we are visiting

	delayBetweenRequests := 2 * time.Second // local variable: how long we pause between each ebook page visit, to be polite to the server

	maxRetriesPerIndex := 3 // local variable: how many times we retry a single index before giving up on it

	retryBackoffBase := 2 * time.Second // local variable: the starting wait time before a retry, which grows with each attempt

	notFoundPhrase := "No ebook by that number." // local variable: the phrase gutenberg shows on its "not found" page

	interruptCtx, stopListening := signal.NotifyContext(context.Background(), os.Interrupt) // create a context that cancels itself when ctrl+c is pressed
	defer stopListening()                                                                   // make sure we stop listening for the signal when main exits

	sharedHttpClient := &http.Client{Timeout: requestTimeout} // create one http client here and reuse it for every request, so tcp connections can be reused

	currentIndex := 0 // this variable tracks which ebook number the loop is currently on

	for pageNumber := 1; ; pageNumber++ { // start counting from 1 and increase forever, no upper limit
		currentIndex = pageNumber // update our tracking variable to match the loop's current position

		if interruptCtx.Err() != nil { // check if ctrl+c was pressed before we even start this iteration
			log.Printf("shutdown requested, stopping cleanly at index %d", currentIndex) // log where we stopped so progress is visible
			break                                                                        // exit the loop without starting a new request
		}

		result, err := fetchWithRetries(interruptCtx, sharedHttpClient, currentIndex, userAgent, notFoundPhrase, maxRetriesPerIndex, retryBackoffBase) // visit the page for the current index, retrying on failure
		if err != nil {                                                                                                                                // check if all retries were exhausted or we were cancelled
			if interruptCtx.Err() != nil { // check specifically whether this error was caused by ctrl+c
				log.Printf("shutdown requested, stopping cleanly at index %d", currentIndex) // log the clean shutdown point
				break                                                                        // exit the loop since the user asked us to stop
			}
			log.Printf("index %d: giving up after %d attempts: %v", currentIndex, maxRetriesPerIndex, err) // log that we gave up on this index entirely
			continue                                                                                       // move on to the next number
		}

		if result.notFound { // check if either the status code or the page text told us this ebook does not exist
			log.Printf("index %d: no ebook found (status %d), stopping loop", currentIndex, result.statusCode) // log that we found the stopping point
			break                                                                                              // exit the for loop completely
		}

		log.Printf("index %d: fetched %d bytes (status %d)", currentIndex, len(result.content), result.statusCode) // log how much content we got for this index

		select { // pause between requests to be polite, but stay interruptible while doing so
		case <-time.After(delayBetweenRequests): // the polite delay passed normally
			// continue to the next loop iteration
		case <-interruptCtx.Done(): // ctrl+c was pressed while we were waiting
			log.Printf("shutdown requested, stopping cleanly at index %d", currentIndex) // log the clean shutdown point
			return                                                                       // exit main immediately since there is nothing left to do
		}
	}

	log.Printf("finished, last index processed: %d", currentIndex) // final summary log line once the loop ends normally
}
