// 日付などの計算をこのコードにまとめる
package main

import (
	"fmt"
	//"net/http"
	//"os"
	"time"
	//"google.golang.org/appengine" // Required external App Engine library
	//"google.golang.org/appengine/log"
)

/*
// 与えられた日付の前日が取引日かどうかを判定する関数
// 実行時の日付time.Now().In(jst)と休日一覧Mapを渡す
func isPreviousBussinessday(r *http.Request, t time.Time, holidayMap map[string]bool) bool {
	ctx := appengine.NewContext(r)

	if ENV == "test" {
		// test環境は常にtrue
		return true
	}

	// 以下はprodの場合
	log.Infof(ctx, "received date: %v", t)
	// 直近の営業日を取得
	previousBussinessDay, err := getPreviousBussinessDay(t, holidayMap)
	if err != nil {
		log.Errorf(ctx, "failed to getPreviousBussinessDay. %v", err)
		os.Exit(0)
	}
	log.Infof(ctx, "previous BussinessDay %s", previousBussinessDay)

	previousTime, _ := time.Parse("2006/01/02", previousBussinessDay)
	previousTimeNextDay := previousTime.AddDate(0, 0, 1)
	if t.Format("2006/01/02") == previousTimeNextDay.Format("2006/01/02") {
		log.Infof(ctx, "previous day is BussinessDay.")
		return true
	}
	return false
}
*/

// 直近の営業日を取得する関数
// 実行時の日付time.Now().In(jst)と休日一覧Mapを渡す
// 東証の休日一覧には土日が入っていないことがあるのでisSaturdayOrSundayで土日でないかも確認する
func getPreviousBussinessDay(t time.Time, holidayMap map[string]bool) (string, error) {
	// 無限ループは嫌なので直近30日間見て取引日が見つからなかったらerrorを返す
	for i := 1; i <= 30; i++ {
		previousTime := t.AddDate(0, 0, -i)
		previousDate := previousTime.Format("2006/01/02")
		if !holidayMap[previousDate] && !isSaturdayOrSunday(previousTime) {
			return previousDate, nil
		}
	}
	return "", fmt.Errorf("there are no previous bussinessdays")
}

func isSaturdayOrSunday(t time.Time) bool {
	day := t.Weekday()
	switch day {
	case 6, 0: // Saturday, Sunday
		return true
	}
	return false
}
