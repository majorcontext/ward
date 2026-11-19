package time

type Duration int64

type Time struct{}

func Now() Time { return Time{} }

func (Time) After(Time) bool { return false }

func Sleep(Duration) {}

func After(Duration) <-chan Time { return nil }
