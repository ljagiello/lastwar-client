// Command checkversion makes the client's anonymous check-version call and prints the gate host,
// table_version (version,size,md5,crc;patch chain) and locale -- the values needed to download
// the current data-table archive (see docs/scouting.mdx).
package main

import (
	"fmt"
	"net/http"
	"time"

	"lastwar-client/internal/gsl"
)

func main() {
	cv, host, err := gsl.CheckVersion(&http.Client{Timeout: 20 * time.Second}, "APS550")
	if err != nil {
		panic(err)
	}
	fmt.Println(host)
	fmt.Println("table_version", cv.TableVersion)
	fmt.Println("locale", cv.Locale)
}
