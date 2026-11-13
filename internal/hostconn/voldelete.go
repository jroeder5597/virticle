package hostconn

import (
	"strings"

	"github.com/digitalocean/go-libvirt"
)

// StorageVolDelete deletes a volume. On an NFS share libvirt removes the file as the user that owns it, not as
// root (it cannot know whether the server squashes root). A disk that a VM used and that was not released
// cleanly (the node was reset) belongs to the VM user, who has no write access to the pool directory, so
// libvirt answers "cannot unlink file: Permission denied" and the file stays. The node command removes it as
// root, inside the pool directories only; it is asked when libvirt refused for that reason.
func (h *Host) StorageVolDelete(v libvirt.StorageVol, flags libvirt.StorageVolDeleteFlags) error {
	err := h.Libvirt.StorageVolDelete(v, flags)
	if err == nil || flags != 0 || !strings.Contains(strings.ToLower(err.Error()), "permission denied") {
		return err
	}
	file, perr := h.Libvirt.StorageVolGetPath(v)
	if perr != nil || file == "" {
		return err
	}
	if rerr := h.Remove(file); rerr != nil {
		return err // keep libvirt's message: it names the file
	}
	return nil
}
