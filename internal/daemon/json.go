package daemon

import (
	"io"

	"paas/internal/strictjson"
)

func decodeDaemonJSON(body io.Reader, output any) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	return strictjson.Decode(data, output)
}
