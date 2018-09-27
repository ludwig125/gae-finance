package main

import (
	"fmt"
	"io/ioutil"
	//"log"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/sheets/v4"
	"google.golang.org/appengine" // Required external App Engine library
	"google.golang.org/appengine/log"
	"google.golang.org/appengine/urlfetch" // 外部にhttpするため
)

func main() {
	http.HandleFunc("/daily", indexHandlerDaily)
	http.HandleFunc("/", indexHandler)
	appengine.Main() // Starts the server to receive requests
}

// Dailyの株価を取得
func indexHandlerDaily(w http.ResponseWriter, r *http.Request) {

	// googleAPIへのclientをリクエストから作成
	client := getClientWithJson(r)

	// GAE log
	ctx := appengine.NewContext(r)

	// spreadsheets clientを取得
	sheetService, err := sheets.New(client)
	if err != nil {
		log.Errorf(ctx, "Unable to retrieve Sheets Client %v", err)
	}

	// spreadsheetから銘柄コードを取得
	codes := readCode(sheetService, r)

	//fmt.Fprintln(w, codes)
	if len(codes) == 0 {
		fmt.Println("No data found.")
	} else {
		for _, row := range codes {
			code := row[0].(string)
			// codeごとに株価を取得
			date_price := doScrapeDaily(r, code)
			for i := len(date_price) - 1; i >= 0; i-- {
				//fmt.Fprintln(w, code, date_price[i])
				// 日次の株価をspreadsheetに書き込み
				writeStockpriceDaily(sheetService, r, code, date_price[i])
			}
			time.Sleep(1 * time.Second) // 1秒待つ
		}
	}
}

// codeごとの株価比率
type code_rate struct {
	Code string
	Rate []float64
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	// googleAPIへのclientをリクエストから作成
	client := getClientWithJson(r)

	// GAE log
	ctx := appengine.NewContext(r)

	// spreadsheets clientを取得
	sheetService, err := sheets.New(client)
	if err != nil {
		log.Errorf(ctx, "Unable to retrieve Sheets Client %v", err)
	}

	if !isBussinessday(sheetService, r) {
		log.Infof(ctx, "Is not a business day today.")
		return
	}

	// spreadsheetから銘柄コードを取得
	codes := readCode(sheetService, r)

	//fmt.Fprintln(w, codes)

	if len(codes) == 0 {
		fmt.Println("No data found.")
	} else {
		for _, row := range codes {
			code := row[0].(string)
			// codeごとに株価を取得
			date, stockprice := doScrape(r, code)
			fmt.Fprintln(w, code, date, stockprice)

			// 株価をspreadsheetに書き込み
			writeStockprice(sheetService, r, code, date, stockprice)

			time.Sleep(1 * time.Second) // 1秒待つ
		}

		// spreadsheetから株価を取得する
		resp := getLatestPrice(sheetService, r)
		//fmt.Fprintln(w, resp)

		//		// codeごとの株価比率
		//		type code_rate struct {
		//			Code string
		//			Rate []float64
		//		}

		// 全codeの株価比率
		var whole_code_rate []code_rate
		for _, row := range codes {
			code := row[0].(string)
			rate := calcIncreaseRate(resp, code)
			whole_code_rate = append(whole_code_rate, code_rate{code, rate})
		}

		// 一つ前との比率が一番大きいもの順にソート
		sort.SliceStable(whole_code_rate, func(i, j int) bool { return whole_code_rate[i].Rate[0] > whole_code_rate[j].Rate[0] })
		fmt.Fprintln(w, whole_code_rate)

		// 事前にrateのシートをclear
		clearRate(sheetService, r)

		// 株価の比率順にソートしたものを書き込み
		writeRate(sheetService, r, whole_code_rate)
	}
}

func getClientWithJson(r *http.Request) *http.Client {
	// リクエストからcontextを作成
	// GAE log
	ctx := appengine.NewContext(r)

	credentialFilePath := "myfinance-01-dc1116b8f354.json"
	data, err := ioutil.ReadFile(credentialFilePath)
	if err != nil {
		log.Errorf(ctx, "Unable to read client secret file: %v", err)
	}
	conf, err := google.JWTConfigFromJSON(data, "https://www.googleapis.com/auth/spreadsheets")
	if err != nil {
		log.Errorf(ctx, "Unable to parse client secret file to config: %v", err)
	}
	return conf.Client(ctx)
}

func isBussinessday(srv *sheets.Service, r *http.Request) bool {
	ctx := appengine.NewContext(r)

	if v := os.Getenv("ENV"); v == "test" {
		// test環境は常にtrue
		return true
	} else if v == "prod" {

		// 土日は実行しない
		jst, _ := time.LoadLocation("Asia/Tokyo")
		now := time.Now().In(jst)

		d := now.Weekday()
		switch d {
		case 6, 0: // Saturday, Sunday
			return false
		}

		today_ymd := now.Format("2006/01/02")

		holidays := getHolidays(srv, r)
		for _, row := range holidays {
			holiday := row[0].(string)
			if holiday == today_ymd {
				log.Infof(ctx, "Today is holiday.")
				return false
			}
		}
	} else {
		// ENVがprodでもtestでもない場合は異常終了
		log.Errorf(ctx, "ENV must be 'test' or 'prod': %v", v)
		os.Exit(0)
	}
	return true

}

func getHolidays(srv *sheets.Service, r *http.Request) [][]interface{} {
	ctx := appengine.NewContext(r)

	// 'holiday' worksheet を読み取り
	// 東京証券取引所の休日: https://www.jpx.co.jp/corporate/calendar/index.html

	sheetId := ""
	// sheetIdを環境変数から読み込む
	if v := os.Getenv("HOLIDAY_SHEETID"); v != "" {
		sheetId = v
	} else {
		log.Errorf(ctx, "Failed to get holiday sheetId. '%v'", v)
		os.Exit(0)
	}
	readRange := "holiday"
	resp, err := srv.Spreadsheets.Values.Get(sheetId, readRange).Do()
	if err != nil {
		log.Errorf(ctx, "Unable to retrieve data from sheet: %v", err)
	}
	status := resp.ServerResponse.HTTPStatusCode
	if status != 200 {
		log.Errorf(ctx, "HTTPstatus error. %v", status)
	}
	return resp.Values
}

func readCode(srv *sheets.Service, r *http.Request) [][]interface{} {
	ctx := appengine.NewContext(r)

	// 'code' worksheet を読み取り

	sheetId := ""
	// sheetIdを環境変数から読み込む
	if v := os.Getenv("CODE_SHEETID"); v != "" {
		sheetId = v
	} else {
		log.Errorf(ctx, "Failed to get codes sheetId. '%v'", v)
		os.Exit(0)
	}
	readRange := "code"
	resp, err := srv.Spreadsheets.Values.Get(sheetId, readRange).Do()
	if err != nil {
		log.Errorf(ctx, "Unable to retrieve data from sheet: %v", err)
	}
	status := resp.ServerResponse.HTTPStatusCode
	if status != 200 {
		log.Errorf(ctx, "HTTPstatus error. %v", status)
	}
	return resp.Values
}

func doScrapeDaily(r *http.Request, code string) [][]string {
	ctx := appengine.NewContext(r)
	client := urlfetch.Client(ctx)

	base_url := ""
	// リクエスト対象のURLを環境変数から読み込む
	if v := os.Getenv("DAILY_PRICE_URL"); v != "" {
		base_url = v
	} else {
		log.Errorf(ctx, "Failed to get daily base_url. '%v'", v)
		os.Exit(0)
	}

	// Request the HTML page.
	url := base_url + code
	//res, err := http.Get(url)
	res, err := client.Get(url)
	if err != nil {
		log.Errorf(ctx, "err: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		log.Errorf(ctx, "status code error: %d %s %s", res.StatusCode, res.Status, url)
	}

	// Load the HTML document
	doc, err := goquery.NewDocumentFromReader(res.Body)
	if err != nil {
		log.Errorf(ctx, "err: %v", err)
	}

	// date と priceを取得
	var date_price [][]string
	doc.Find(".m-tableType01_table table tbody tr").Each(func(i int, s *goquery.Selection) {
		date := s.Find(".a-taC").Text()
		re := regexp.MustCompile(`[0-9]+/[0-9]+`).Copy()
		date = re.FindString(date)

		var arr []string
		arr = append(arr, date)
		s.Find(".a-taR").Each(func(j int, s2 *goquery.Selection) {
			// ","を取り除く
			arr = append(arr, strings.Replace(s2.Text(), ",", "", -1))
		})
		date_price = append(date_price, arr)
	})
	return date_price
}

func doScrape(r *http.Request, code string) (string, string) {
	ctx := appengine.NewContext(r)
	client := urlfetch.Client(ctx)

	base_url := ""
	// リクエスト対象のURLを環境変数から読み込む
	if v := os.Getenv("HOURLY_PRICE_URL"); v != "" {
		base_url = v
	} else {
		log.Errorf(ctx, "Failed to get base_url. '%v'", v)
		os.Exit(0)
	}

	// Request the HTML page.
	url := base_url + code
	//res, err := http.Get(url)
	res, err := client.Get(url)
	if err != nil {
		log.Errorf(ctx, "err: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		log.Errorf(ctx, "status code error: %d %s %s", res.StatusCode, res.Status, url)
	}

	// Load the HTML document
	doc, err := goquery.NewDocumentFromReader(res.Body)
	if err != nil {
		log.Errorf(ctx, "err: %v", err)
	}

	// time と priceを取得
	var time, price string
	doc.Find(".stockInfoinner").Each(func(i int, s *goquery.Selection) {
		time = s.Find(".ttl1").Text()
		price = s.Find(".item1").Text()
	})
	// 必要な形に整形して返す
	return getFormatedDate(time), getFormatedPrice(price)
}

func getFormatedDate(s string) string {
	re := regexp.MustCompile(`\d+:\d+`).Copy()
	t := strings.Split(re.FindString(s), ":") // ["06", "00"]
	data_hour, _ := strconv.Atoi(t[0])
	data_hour = data_hour + 9 // GMT -> JST
	data_min := t[1]

	jst, _ := time.LoadLocation("Asia/Tokyo")
	now := time.Now().In(jst)

	d := now.Weekday()
	h := now.Hour()

	var ymd string
	switch d {
	case 1: // Monday
		if h < data_hour {
			// 月曜に取得したデータが現在時刻より後であればそれは前の金曜のもの
			ymd = now.AddDate(0, 0, -3).Format("2006/01/02")
		} else {
			ymd = now.Format("2006/01/02")
		}
	case 2, 3, 4, 5: // Tuesday,..Friday
		if h < data_hour {
			// 火~金曜に取得したデータが現在時刻より後であればそれは前日のもの
			ymd = now.AddDate(0, 0, -1).Format("2006/01/02")
		} else {
			ymd = now.Format("2006/01/02")
		}
	case 6: // Saturday
		// 土曜に取得したデータは前の金曜のもの
		ymd = now.AddDate(0, 0, -1).Format("2006/01/02")
	case 0: // Sunday
		// 日曜に取得したデータは前の金曜のもの
		ymd = now.AddDate(0, 0, -2).Format("2006/01/02")
	}
	return fmt.Sprintf("%s %2d:%s", ymd, data_hour, data_min)
}

func getFormatedPrice(s string) string {
	re := regexp.MustCompile(`[0-9,.]+`).Copy()
	price := re.FindString(s)
	price = strings.Replace(price, ".0", "", 1)
	price = strings.Replace(price, ",", "", -1)
	return price
}

func writeStockpriceDaily(srv *sheets.Service, r *http.Request, code string, dp []string) {
	ctx := appengine.NewContext(r)

	sheetId := ""
	// sheetIdを環境変数から読み込む
	if v := os.Getenv("STOCKPRICE_SHEETID"); v != "" {
		sheetId = v
	} else {
		log.Errorf(ctx, "Failed to get stockprice sheetId. '%v'", v)
		os.Exit(0)
	}
	writeRange := "daily"

	valueRange := &sheets.ValueRange{
		MajorDimension: "ROWS",
		Values: [][]interface{}{
			// code, 日付, 始値, 高値, 安値, 終値, 売買高, 修正後終値
			[]interface{}{code, dp[0], dp[1], dp[2], dp[3], dp[4], dp[5], dp[6]},
		},
	}
	resp, err := srv.Spreadsheets.Values.Append(sheetId, writeRange, valueRange).ValueInputOption("USER_ENTERED").InsertDataOption("INSERT_ROWS").Do()
	if err != nil {
		log.Errorf(ctx, "Unable to write value. %v", err)
	}
	status := resp.ServerResponse.HTTPStatusCode
	if status != 200 {
		log.Errorf(ctx, "HTTPstatus error. %v", status)
	}
}

func writeStockprice(srv *sheets.Service, r *http.Request, code string, date string, stockprice string) {
	ctx := appengine.NewContext(r)

	sheetId := ""
	// sheetIdを環境変数から読み込む
	if v := os.Getenv("STOCKPRICE_SHEETID"); v != "" {
		sheetId = v
	} else {
		log.Errorf(ctx, "Failed to get stockprice sheetId. '%v'", v)
		os.Exit(0)
	}
	writeRange := "stockprice"

	valueRange := &sheets.ValueRange{
		MajorDimension: "ROWS",
		Values: [][]interface{}{
			[]interface{}{code, date, stockprice},
		},
	}
	resp, err := srv.Spreadsheets.Values.Append(sheetId, writeRange, valueRange).ValueInputOption("USER_ENTERED").InsertDataOption("INSERT_ROWS").Do()
	if err != nil {
		log.Errorf(ctx, "Unable to write value. %v", err)
	}
	status := resp.ServerResponse.HTTPStatusCode
	if status != 200 {
		log.Errorf(ctx, "HTTPstatus error. %v", status)
	}
}

func getLatestPrice(srv *sheets.Service, r *http.Request) [][]interface{} {
	ctx := appengine.NewContext(r)

	sheetId := ""
	// sheetIdを環境変数から読み込む
	if v := os.Getenv("STOCKPRICE_SHEETID"); v != "" {
		sheetId = v
	} else {
		log.Errorf(ctx, "Failed to get stockprice sheetId. '%v'", v)
		os.Exit(0)
	}
	readRange := "stockprice"
	// stockpriceシートからデータを取得
	resp, err := srv.Spreadsheets.Values.Get(sheetId, readRange).Do()
	if err != nil {
		log.Errorf(ctx, "Unable to retrieve data from sheet: %v", err)
	}
	status := resp.ServerResponse.HTTPStatusCode
	if status != 200 {
		log.Errorf(ctx, "HTTPstatus error. %v", status)
	}
	v := reflect.ValueOf(resp)
	log.Debugf(ctx, "type: %v", v.Type())
	log.Debugf(ctx, resp.Range)
	log.Debugf(ctx, "len resp.Values: %v", len(resp.Values)) //これでデータの全行数が取れる
	return resp.Values
}

func calcIncreaseRate(val [][]interface{}, code string) []float64 {
	DATA_NUM := 7

	var price []float64
	// 後ろから順番に読んでいく
	count := DATA_NUM
	for i := 0; i < len(val); i++ {
		v := val[len(val)-1-i] // [8316 2018/08/09 15:00 4426]
		if v[0] == code {
			p, _ := strconv.ParseFloat(v[2].(string), 64)
			price = append(price, p)
			//va := reflect.ValueOf(v[2].(string)) // 型確認
			//log.Println(va.Type(), v[2])

			count = count - 1
			// 指定回数分取得したらループを抜ける
			if count <= 0 {
				break
			}
		}
	}
	// price ex. [685.1 684.6 683.2 684.2 686.9 684.3 684.3]
	//log.Println(code, price)

	// rate[0] = price[0]/price[1]
	// ...
	// rate[DATA_NUM-2] = price[DATA_NUM-2]/price[DATA_NUM-1]
	var rate []float64
	for i := 0; i < DATA_NUM-1; i++ {
		if i+1 < len(price) {
			rate = append(rate, price[0]/price[i+1])
		} else {
			rate = append(rate, 0.0)
		}
	}
	//log.Println(code, rate)
	// rate ex. [1.0007303534910896 1.002781030444965 1.0013154048523822 0.997379531227253 1.0011690778898146 1.0011690778898146]
	return rate
}

func clearRate(srv *sheets.Service, r *http.Request) {
	ctx := appengine.NewContext(r)

	sheetId := ""
	// sheetIdを環境変数から読み込む
	if v := os.Getenv("RATE_SHEETID"); v != "" {
		sheetId = v
	} else {
		log.Errorf(ctx, "Failed to get price rate sheetId. '%v'", v)
		os.Exit(0)
	}
	writeRange := "rate"

	// clear stockprice rate spreadsheet:
	resp, err := srv.Spreadsheets.Values.Clear(sheetId, writeRange, &sheets.ClearValuesRequest{}).Do()
	if err != nil {
		log.Errorf(ctx, "Unable to clear value. %v", err)
	}
	status := resp.ServerResponse.HTTPStatusCode
	if status != 200 {
		log.Errorf(ctx, "HTTPstatus error. %v", status)
	}
}

func writeRate(srv *sheets.Service, r *http.Request, rate []code_rate) {
	ctx := appengine.NewContext(r)

	sheetId := ""
	// sheetIdを環境変数から読み込む
	if v := os.Getenv("RATE_SHEETID"); v != "" {
		sheetId = v
	} else {
		log.Errorf(ctx, "Failed to get price rate sheetId. '%v'", v)
		os.Exit(0)
	}
	writeRange := "rate"

	// spreadsheetに書き込み対象の行列を作成
	matrix := make([][]interface{}, len(rate))
	// 株価の比率順にソートしたものを書き込み
	for i, r := range rate {
		matrix[i] = []interface{}{r.Code, r.Rate[0], r.Rate[1], r.Rate[2], r.Rate[3], r.Rate[4], r.Rate[5]}
	}

	valueRange := &sheets.ValueRange{
		MajorDimension: "ROWS",
		Values:         matrix,
	}
	// Write stockprice rate spreadsheet:
	resp, err := srv.Spreadsheets.Values.Append(sheetId, writeRange, valueRange).ValueInputOption("USER_ENTERED").InsertDataOption("INSERT_ROWS").Do()
	if err != nil {
		log.Errorf(ctx, "Unable to write value. %v", err)
	}
	status := resp.ServerResponse.HTTPStatusCode
	if status != 200 {
		log.Errorf(ctx, "HTTPstatus error. %v", status)
	}
}
