package ninja

import (
	"github.com/nps5696/go-ninja/model"
)

//
// Device implementors should provide an implementation of this interface
// for each device a driver discovers.
//
// FIXME: consider adding a DeviceSupport object
//
type Device interface {
	GetDriver() Driver
	GetDeviceInfo() *model.Device
	SetEventHandler(func(event string, payload interface{}) error)
}
