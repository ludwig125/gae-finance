package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/sheets/v4"
	"google.golang.org/appengine"          // Required external App Engine library
	"google.golang.org/appengine/urlfetch" // 外部にhttpするため
)

func main() {
	http.HandleFunc("/", indexHandler)
	appengine.Main() // Starts the server to receive requests
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	// googleAPIへのclientをリクエストから作成
	client := getClientWithJson(r)

	// spreadsheets clientを取得
	sheetService, err := sheets.New(client)
	if err != nil {
		log.Fatalf("Unable to retrieve Sheets Client %v", err)
	}

	// spreadsheetから銘柄コードを取得
	codes := readCode(sheetService)

	//	fmt.Fprintln(w, codes)

	if len(codes) == 0 {
		fmt.Println("No data found.")
	} else {
		for _, row := range codes {
			code := row[0].(string)
			// codeごとに株価を取得
			date, stockprice := doScrape(r, code)
			fmt.Fprintln(w, code, date, stockprice)
			// 株価をspreadsheetに書き込み
			writeStockprice(sheetService, code, date, stockprice)
		}

		// spreadsheetから株価を取得する
		resp := getLatestPrice(sheetService)
		//fmt.Fprintln(w, resp)

		// codeごとの株価比率
		type code_rate struct {
			Code string
			Rate []float64
		}

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
		clearRate(sheetService)

		// 株価の比率順にソートしたものを書き込み
		for _, r := range whole_code_rate {
			writeRate(sheetService, r.Code, r.Rate)
		}
	}
}

func getClientWithJson(r *http.Request) *http.Client {
	credentialFilePath := "myfinance-01-dc1116b8f354.json"
	data, err := ioutil.ReadFile(credentialFilePath)
	if err != nil {
		log.Fatalf("Unable to read client secret file: %v", err)
	}
	conf, err := google.JWTConfigFromJSON(data, "https://www.googleapis.com/auth/spreadsheets")
	if err != nil {
		log.Fatalf("Unable to parse client secret file to config: %v", err)
	}
	// リクエストからcontextを作成
	ctx := appengine.NewContext(r)
	return conf.Client(ctx)
}

func readCode(srv *sheets.Service) [][]interface{} {
	// 'code' worksheet を読み取り
	spreadsheetId := "1ExUKJy5SfKb62wycg1jOiHHeQ1t3hGyE2Vau5RkKzfk"
	readRange := "code"
	resp, err := srv.Spreadsheets.Values.Get(spreadsheetId, readRange).Do()
	if err != nil {
		log.Fatalf("Unable to retrieve data from sheet: %v", err)
	}
	status := resp.ServerResponse.HTTPStatusCode
	if status != 200 {
		log.Fatalf("HTTPstatus error. %v", status)
	}
	return resp.Values
}

func doScrape(r *http.Request, code string) (string, string) {
	ctx := appengine.NewContext(r)
	client := urlfetch.Client(ctx)
	// Request the HTML page.
	url := "https://www.nikkei.com/smartchart/?code=" + code
	//res, err := http.Get(url)
	res, err := client.Get(url)
	if err != nil {
		log.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		log.Fatalf("status code error: %d %s", res.StatusCode, res.Status)
	}

	// Load the HTML document
	doc, err := goquery.NewDocumentFromReader(res.Body)
	if err != nil {
		log.Fatal(err)
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

func writeStockprice(srv *sheets.Service, code string, date string, stockprice string) {
	// Write stockprice spreadsheet:
	writespreadsheetId := "1FcwyVrMIZ5xGrFaJvIg0SVpJPsf9Q7WabVxBXRxpUZA"
	writeRange := "stockprice"

	valueRange := &sheets.ValueRange{
		MajorDimension: "ROWS",
		Values: [][]interface{}{
			[]interface{}{code, date, stockprice},
		},
	}
	resp, err := srv.Spreadsheets.Values.Append(writespreadsheetId, writeRange, valueRange).ValueInputOption("USER_ENTERED").InsertDataOption("INSERT_ROWS").Do()
	if err != nil {
		log.Fatalf("Unable to write value. %v", err)
	}
	status := resp.ServerResponse.HTTPStatusCode
	if status != 200 {
		log.Fatalf("HTTPstatus error. %v", status)
	}
}

func getLatestPrice(srv *sheets.Service) [][]interface{} {
	// stockpriceシートからデータを取得
	spreadsheetId := "1FcwyVrMIZ5xGrFaJvIg0SVpJPsf9Q7WabVxBXRxpUZA"
	readRange := "stockprice"
	resp, err := srv.Spreadsheets.Values.Get(spreadsheetId, readRange).Do()
	if err != nil {
		log.Fatalf("Unable to retrieve data from sheet: %v", err)
	}
	status := resp.ServerResponse.HTTPStatusCode
	if status != 200 {
		log.Fatalf("HTTPstatus error. %v", status)
	}
	v := reflect.ValueOf(resp)
	log.Println(v.Type())
	log.Println(resp.Range)
	log.Println(len(resp.Values)) //これでデータの全行数が取れる
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

func clearRate(srv *sheets.Service) {
	// clear stockprice rate spreadsheet:
	writespreadsheetId := "1ZQK1SdjLS0ZCrKL_0A2jrbG-nxEcf-h4UIDgXAXCfMM"
	writeRange := "rate"

	resp, err := srv.Spreadsheets.Values.Clear(writespreadsheetId, writeRange, &sheets.ClearValuesRequest{}).Do()
	if err != nil {
		log.Fatalf("Unable to clear value. %v", err)
	}
	status := resp.ServerResponse.HTTPStatusCode
	if status != 200 {
		log.Fatalf("HTTPstatus error. %v", status)
	}
}

func writeRate(srv *sheets.Service, code string, rate []float64) {
	// Write stockprice rate spreadsheet:
	writespreadsheetId := "1ZQK1SdjLS0ZCrKL_0A2jrbG-nxEcf-h4UIDgXAXCfMM"
	writeRange := "rate"

	valueRange := &sheets.ValueRange{
		MajorDimension: "ROWS",
		Values: [][]interface{}{
			[]interface{}{code, rate[0], rate[1], rate[2], rate[3], rate[4], rate[5]},
		},
	}
	resp, err := srv.Spreadsheets.Values.Append(writespreadsheetId, writeRange, valueRange).ValueInputOption("USER_ENTERED").InsertDataOption("INSERT_ROWS").Do()
	if err != nil {
		log.Fatalf("Unable to write value. %v", err)
	}
	status := resp.ServerResponse.HTTPStatusCode
	if status != 200 {
		log.Fatalf("HTTPstatus error. %v", status)
	}
}
