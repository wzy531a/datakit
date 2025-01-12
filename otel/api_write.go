package otelcol

import (
	"net/http"

	"github.com/GuanceCloud/cliutils/point"
	"gitlab.jiagouyun.com/cloudcare-tools/datakit/internal/httpapi"
)

type OtelAPIWrite interface {
	Feed(*http.Request, point.Category, []*point.Point) error
}

func ApiWrite(c http.ResponseWriter, req *http.Request, x ...interface{}) (interface{}, error) {
	if x == nil || len(x) != 1 {
		return nil, httpapi.ErrInvalidAPIHandler
	}

	h, ok := x[0].(OtelAPIWrite)
	if !ok {
		return nil, httpapi.ErrInvalidAPIHandler
	}

	wr := httpapi.GetAPIWriteResult()
	defer httpapi.PutAPIWriteResult(wr)

	if err := wr.APIV1Write(req); err != nil {
		return nil, err
	} else {
		if len(wr.Points) != 0 {
			if err := h.Feed(req, wr.Category, wr.Points); err != nil {
				// ignore err
			}
		}

		return wr.RespBody, nil
	}
}
