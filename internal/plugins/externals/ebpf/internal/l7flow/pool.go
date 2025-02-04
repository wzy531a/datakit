//go:build linux
// +build linux

package l7flow

import (
	"sync"

	"gitlab.jiagouyun.com/cloudcare-tools/datakit/internal/plugins/externals/ebpf/internal/l7flow/comm"
)

const (
	netDataSize64  = 64
	netDataSize128 = 128
	netDataSize256 = 256
	netDataSize512 = 512
	netDataSize1k  = 1024
	netDataSize2k  = 2048
	netDataSize4k  = 4096
)

var (
	netwrksyncPool64  = newNetDataPool(netDataSize64)
	netwrksyncPool128 = newNetDataPool(netDataSize128)
	netwrksyncPool256 = newNetDataPool(netDataSize256)
	netwrksyncPool512 = newNetDataPool(netDataSize512)
	netwrksyncPool1k  = newNetDataPool(netDataSize1k)
	netwrksyncPool2k  = newNetDataPool(netDataSize2k)
	netwrksyncPool4k  = newNetDataPool(netDataSize4k)
)

func newNetDataPool(size int) *sync.Pool {
	return &sync.Pool{
		New: func() interface{} {
			return &comm.NetwrkData{
				Payload: make([]byte, 0, size),
			}
		},
	}
}

func getNetwrkData(bufLen int) *comm.NetwrkData {
	switch {
	case bufLen <= netDataSize64:
		return netwrksyncPool64.Get().(*comm.NetwrkData)
	case bufLen <= netDataSize128:
		return netwrksyncPool128.Get().(*comm.NetwrkData)
	case bufLen <= netDataSize256:
		return netwrksyncPool256.Get().(*comm.NetwrkData)
	case bufLen <= netDataSize512:
		return netwrksyncPool512.Get().(*comm.NetwrkData)
	case bufLen <= netDataSize1k:
		return netwrksyncPool1k.Get().(*comm.NetwrkData)
	case bufLen <= netDataSize2k:
		return netwrksyncPool2k.Get().(*comm.NetwrkData)
	case bufLen <= netDataSize4k:
		return netwrksyncPool4k.Get().(*comm.NetwrkData)
	default:
		return nil
	}
}

func putNetwrkData(data *comm.NetwrkData) {
	if data == nil {
		return
	}

	data = resetNetwrkData(data)

	switch {
	case cap(data.Payload) <= netDataSize64:
		netwrksyncPool64.Put(data)
	case cap(data.Payload) <= netDataSize128:
		netwrksyncPool128.Put(data)
	case cap(data.Payload) <= netDataSize256:
		netwrksyncPool256.Put(data)
	case cap(data.Payload) <= netDataSize512:
		netwrksyncPool512.Put(data)
	case cap(data.Payload) <= netDataSize1k:
		netwrksyncPool1k.Put(data)
	case cap(data.Payload) <= netDataSize2k:
		netwrksyncPool2k.Put(data)
	case cap(data.Payload) <= netDataSize4k:
		netwrksyncPool4k.Put(data)
	default:
	}
}

func resetNetwrkData(data *comm.NetwrkData) *comm.NetwrkData {
	data.Conn = comm.ConnectionInfo{}
	data.FnCallSize = 0
	data.CaptureSize = 0
	data.TCPSeq = 0
	data.Thread = [2]int32{}
	data.TS = 0
	data.TSTail = 0
	data.Index = 0
	data.Fn = 0
	data.Payload = data.Payload[:0]

	return data
}
