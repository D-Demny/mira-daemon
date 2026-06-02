package bluetooth

import (
	"testing"

	"github.com/godbus/dbus/v5"
)

// formatDevicePath: MAC -> BlueZ object path. frontend's useBluetooth does the inverse

func TestFormatDevicePath_StandardMAC(t *testing.T) {
	t.Parallel()

	got := formatDevicePath("/org/bluez/hci0", "AA:BB:CC:DD:EE:FF")
	want := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF")
	if got != want {
		t.Errorf("formatDevicePath: got %q want %q", got, want)
	}
}

func TestFormatDevicePath_AlternateAdapter(t *testing.T) {
	t.Parallel()

	got := formatDevicePath("/org/bluez/hci1", "11:22:33:44:55:66")
	want := dbus.ObjectPath("/org/bluez/hci1/dev_11_22_33_44_55_66")
	if got != want {
		t.Errorf("formatDevicePath: got %q want %q", got, want)
	}
}

// fillDeviceInfo: BlueZ property dict -> DeviceInfo

func TestFillDeviceInfo_EmptyPropsMapLeavesAllFieldsZero(t *testing.T) {
	t.Parallel()

	info := &DeviceInfo{}
	fillDeviceInfo(info, map[string]dbus.Variant{})

	if info.Name != "" || info.Alias != "" || info.Class != "" || info.Icon != "" {
		t.Errorf("string fields should be empty: %+v", info)
	}
	if info.Paired || info.Trusted || info.Blocked || info.Connected || info.LegacyPairing {
		t.Errorf("bool fields should all be false: %+v", info)
	}
}

func TestFillDeviceInfo_AllStringFieldsPopulatedFromVariants(t *testing.T) {
	t.Parallel()

	info := &DeviceInfo{}
	fillDeviceInfo(info, map[string]dbus.Variant{
		"Name":  dbus.MakeVariant("Pixel 7"),
		"Alias": dbus.MakeVariant("Test Phone"),
		"Icon":  dbus.MakeVariant("phone"),
	})

	if got, want := info.Name, "Pixel 7"; got != want {
		t.Errorf("Name: got %q want %q", got, want)
	}
	if got, want := info.Alias, "Test Phone"; got != want {
		t.Errorf("Alias: got %q want %q", got, want)
	}
	if got, want := info.Icon, "phone"; got != want {
		t.Errorf("Icon: got %q want %q", got, want)
	}
}

func TestFillDeviceInfo_AllBoolFieldsPopulatedFromVariants(t *testing.T) {
	t.Parallel()

	info := &DeviceInfo{}
	fillDeviceInfo(info, map[string]dbus.Variant{
		"Paired":        dbus.MakeVariant(true),
		"Trusted":       dbus.MakeVariant(true),
		"Blocked":       dbus.MakeVariant(false),
		"Connected":     dbus.MakeVariant(true),
		"LegacyPairing": dbus.MakeVariant(false),
	})

	if !info.Paired || !info.Trusted || info.Blocked || !info.Connected || info.LegacyPairing {
		t.Errorf("bool fields incorrectly populated: %+v", info)
	}
}

func TestFillDeviceInfo_ClassUint32IsFormattedAsDecimalString(t *testing.T) {
	t.Parallel()

	// 0x02020C (iPhone-PAN gate value) -> "131596", stored as string
	info := &DeviceInfo{}
	fillDeviceInfo(info, map[string]dbus.Variant{
		"Class": dbus.MakeVariant(uint32(0x02020C)),
	})

	if got, want := info.Class, "131596"; got != want {
		t.Errorf("Class: got %q want %q", got, want)
	}
}

func TestFillDeviceInfo_WrongTypeOnAStringFieldLeavesItEmpty(t *testing.T) {
	t.Parallel()

	// wrong type should silently leave the field empty
	info := &DeviceInfo{}
	fillDeviceInfo(info, map[string]dbus.Variant{
		"Name": dbus.MakeVariant(int32(42)),
	})

	if info.Name != "" {
		t.Errorf("Name should be empty when variant is wrong type, got %q", info.Name)
	}
}

func TestFillDeviceInfo_WrongTypeOnClassLeavesItEmpty(t *testing.T) {
	t.Parallel()

	info := &DeviceInfo{}
	fillDeviceInfo(info, map[string]dbus.Variant{
		"Class": dbus.MakeVariant("not a number"),
	})

	if info.Class != "" {
		t.Errorf("Class should be empty when variant is non-uint32, got %q", info.Class)
	}
}

func TestFillDeviceInfo_DoesNotTouchAddressOrBatteryFields(t *testing.T) {
	t.Parallel()

	// Address + Battery are populated elsewhere, must survive a fillDeviceInfo call
	info := &DeviceInfo{
		Address:           "AA:BB:CC:DD:EE:FF",
		BatteryPercentage: 75,
	}
	fillDeviceInfo(info, map[string]dbus.Variant{
		"Name": dbus.MakeVariant("Pixel"),
	})

	if got, want := info.Address, "AA:BB:CC:DD:EE:FF"; got != want {
		t.Errorf("Address: got %q want %q (must not be touched by fillDeviceInfo)", got, want)
	}
	if got, want := info.BatteryPercentage, 75; got != want {
		t.Errorf("BatteryPercentage: got %d want %d", got, want)
	}
}
