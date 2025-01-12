package otelcol

import (
	"context"
	uhttp "github.com/GuanceCloud/cliutils/network/http"
	"github.com/didip/tollbooth/v6"
	"github.com/didip/tollbooth/v6/limiter"
	"github.com/gin-gonic/gin"
	"gitlab.jiagouyun.com/cloudcare-tools/datakit/internal/httpapi"
	"net/http"
)

// RawHTTPWrapper warp HTTP APIs that:
//   - with rate limit
func RawHTTPWrapper(lmt *limiter.Limiter, next httpapi.APIHandler, other ...interface{}) gin.HandlerFunc {
	return func(c *gin.Context) {
		if lmt != nil {
			if isBlocked(lmt, c.Writer, c.Request) {
				uhttp.HttpErr(c, httpapi.ErrReachLimit)
				lmt.ExecOnLimitReached(c.Writer, c.Request)

				c.Abort()
				return
			}
		}

		ctx, cancel := context.WithCancel(c.Request.Context())
		defer cancel()
		for _, p := range c.Params {
			ctx = context.WithValue(ctx, httpapi.Param(p.Key), p.Value)
		}

		if res, err := next(c.Writer, c.Request.WithContext(ctx), other...); err != nil {
			uhttp.HttpErr(c, err)
		} else {
			httpapi.OK.HttpBody(c, res)
		}
	}
}

func isBlocked(lmt *limiter.Limiter, w http.ResponseWriter, r *http.Request) bool {
	if lmt == nil {
		return false
	}

	return tollbooth.LimitByRequest(lmt, w, r) != nil
}
