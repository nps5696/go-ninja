package channels

type MotionDevice interface {
}

type MotionChannel struct {
	baseChannel
	device MotionDevice
}

func NewMotionChannel(device MotionDevice) *MotionChannel {
	return &MotionChannel{baseChannel{}, device}
}

func (c *MotionChannel) SendMotion() error {
	return c.SendEvent("state", true)
}
