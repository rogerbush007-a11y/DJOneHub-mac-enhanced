#include <stddef.h>

int libusb_init(void **ctx) { return -1; }
void libusb_exit(void *ctx) {}
void *libusb_open_device_with_vid_pid(void *ctx, unsigned short vendor_id, unsigned short product_id) { return NULL; }
void libusb_close(void *dev_handle) {}
int libusb_claim_interface(void *dev_handle, int interface_number) { return -1; }
int libusb_release_interface(void *dev_handle, int interface_number) { return -1; }
int libusb_bulk_transfer(void *dev_handle, unsigned char endpoint, unsigned char *data, int length, int *transferred, unsigned int timeout) { return -1; }
int libusb_get_active_config_descriptor(void *dev, void **config) { return -1; }
void libusb_free_config_descriptor(void *config) {}
void *libusb_get_device(void *dev_handle) { return NULL; }
const char *libusb_error_name(int errcode) { return "ERROR"; }
