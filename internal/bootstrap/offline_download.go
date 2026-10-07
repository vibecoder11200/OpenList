package bootstrap

import (
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/offline_download/tool"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

// InitOfflineDownloadTools initializes every registered tool once. The
// bundled download daemons (aria2, qBittorrent, Transmission) are started by
// runit in parallel with the server, so a tool whose daemon is not listening
// yet fails its first init and would otherwise stay unavailable until an
// admin re-saved its settings. Failed tools are retried in the background so
// they become ready as soon as their daemon is up.
func InitOfflineDownloadTools() {
	var failed []tool.Tool
	for k, v := range tool.Tools {
		res, err := v.Init()
		if err != nil {
			utils.Log.Warnf("init offline download tool %s failed: %s", k, err)
			failed = append(failed, v)
		} else {
			utils.Log.Infof("init offline download tool %s success: %s", k, res)
		}
	}
	if len(failed) == 0 {
		return
	}
	go func() {
		const attempts = 10
		for i := 0; i < attempts; i++ {
			time.Sleep(30 * time.Second)
			var stillFailing []tool.Tool
			for _, t := range failed {
				if t.IsReady() {
					continue
				}
				res, err := t.Init()
				if err != nil {
					utils.Log.Warnf("retry init offline download tool %s failed (%d/%d): %s", t.Name(), i+1, attempts, err)
					stillFailing = append(stillFailing, t)
				} else {
					utils.Log.Infof("retry init offline download tool %s success: %s", t.Name(), res)
				}
			}
			if len(stillFailing) == 0 {
				return
			}
			failed = stillFailing
		}
	}()
}
