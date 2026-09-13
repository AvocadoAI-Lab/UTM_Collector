// syslog-gen 產生 Pico-UTM 格式的測試 Syslog 事件並以 UDP 送出，供驗收與跨機器測試使用。
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"time"
)

func main() {
	target := flag.String("target", "127.0.0.1:5514", "目標 host:port")
	n := flag.Int("n", 1000, "送出筆數")
	rate := flag.Int("rate", 500, "每秒筆數（0 = 不限速）")
	startID := flag.Int64("start-id", 0, "第一筆 event_id（0 = 依目前時間產生，避免與先前的執行重複）")
	kind := flag.String("type", "dpi", "事件類型：dpi（ips / wg 交替，皆含 event_id）或 mixed（user_act、ips、wg、rapid 輪流）")
	host := flag.String("hostname", "Pico-UTM-SYSLOG-GEN", "Syslog 標頭中的 hostname")
	flag.Parse()

	if *n <= 0 || *rate < 0 || (*kind != "dpi" && *kind != "mixed") {
		fmt.Fprintln(os.Stderr, "參數錯誤：-n 需大於 0、-rate 需大於等於 0、-type 需為 dpi 或 mixed")
		os.Exit(2)
	}
	if *startID == 0 {
		*startID = time.Now().Unix() * 100000 // 與實機 event_id 相同的 15 位數格式
	}
	conn, err := net.Dial("udp", *target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "無法連線到 %s：%v\n", *target, err)
		os.Exit(1)
	}
	defer conn.Close()

	begin := time.Now()
	for i := 0; i < *n; i++ {
		if *rate > 0 {
			if d := time.Until(begin.Add(time.Duration(i) * time.Second / time.Duration(*rate))); d > 0 {
				time.Sleep(d)
			}
		}
		if _, err := conn.Write(message(*kind, i, *startID+int64(i), *host)); err != nil {
			fmt.Fprintf(os.Stderr, "第 %d 筆送出失敗：%v\n", i+1, err)
			os.Exit(1)
		}
	}
	elapsed := time.Since(begin)
	fmt.Printf("已送出 %d 筆到 %s（type=%s，event_id %015d–%015d，耗時 %s，%.0f 筆/秒）\n",
		*n, *target, *kind, *startID, *startID+int64(*n)-1, elapsed.Round(time.Millisecond), float64(*n)/max(elapsed.Seconds(), 0.001))
}

func message(kind string, i int, id int64, host string) []byte {
	now := time.Now()
	stamp, ts := now.Format(time.Stamp), now.Unix()
	variant := i%2 + 1 // dpi：ips / wg 交替
	if kind == "mixed" {
		variant = i % 4
	}
	switch variant {
	case 0:
		return fmt.Appendf(nil, `<14>%s %s user_act[2554]: {"ts":%d,"user":"syslog-gen","src":"192.168.1.200","msg":"Sign in (syslog-gen #%d)"}`,
			stamp, host, ts, i+1)
	case 1:
		return fmt.Appendf(nil, `<14>%s %s dpi_event[2554]: {"action":"Block","aid":0,"category":"unauthorized_access","dip":"208.94.116.246","domain":"malware.wicar.org","dport":80,"event_id":"%015d","group_id":0,"initial_from_wan":0,"lan_wan_direction":1,"mac":"D8:BB:C1:D5:49:AD","module":"ips","msg":"syslog-gen test event","osi4_proto":"tcp","osi7_proto":"http","path":"/","severity":"Critical","sid":"8100496005","sip":"192.168.1.200","sockfd":0,"sport":59435,"ts":"%d","type":"SIG","wan_region":"US"}`,
			stamp, host, id, ts)
	case 2:
		return fmt.Appendf(nil, `<14>%s %s dpi_event[2554]: {"action":"Rdpage","dip":"89.238.73.97","domain":"secure.eicar.org","dport":443,"event_id":"%015d","group_id":0,"hit_by":"URL","initial_from_wan":0,"lan_wan_direction":1,"mac":"D8:BB:C1:D5:49:AD","module":"wg","osi4_proto":"tcp","osi7_proto":"https","severity":"Low","sid":"1100000000014","sig_src":"CLOUD","sip":"192.168.1.200","sockfd":0,"sport":62262,"ts":"%d","wan_region":"DE","web_cid":0,"wg_cid":11}`,
			stamp, host, id, ts)
	default:
		return fmt.Appendf(nil, "<11>%s %s rapid[2554]: Abort 404 NotFoundError: from 127.0.0.1 [system] GET /apis/v1/cloud_scanned_devices/SYSLOGGEN%06d\nDevice not found",
			stamp, host, i+1)
	}
}
