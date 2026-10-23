import os
import tempfile
import unittest

from gmp.clients.ssh import KeyStore, Ssh


class SshTests(unittest.TestCase):
    def test_keystore_modes_and_rewrite(self):
        with tempfile.TemporaryDirectory() as d:
            ks = KeyStore(os.path.join(d, "keys"))
            p = ks.write("target", "-----BEGIN KEY-----\nabc\n-----END KEY-----")
            self.assertEqual(oct(os.stat(p).st_mode & 0o777), "0o600")
            self.assertEqual(oct(os.stat(ks.dir).st_mode & 0o777), "0o700")
            p2 = ks.write("target", "-----BEGIN KEY-----\nxyz\n-----END KEY-----")
            self.assertEqual(p, p2)
            with open(p2) as f:
                self.assertIn("xyz", f.read())
            ks.shred("target")
            self.assertFalse(p.exists())

    def test_ssh_argv(self):
        s = Ssh("1.2.3.4", "cb-user", "/k/t.pem", port=2222, known_hosts="/r/kh")
        argv = s.ssh_argv("true")
        self.assertEqual(argv[0], "ssh")
        self.assertIn("-p", argv); self.assertIn("2222", argv)
        self.assertIn("IdentitiesOnly=yes", argv); self.assertIn("BatchMode=yes", argv)
        self.assertIn("UserKnownHostsFile=/r/kh", argv)
        self.assertEqual(argv[-2:], ["cb-user@1.2.3.4", "true"])
