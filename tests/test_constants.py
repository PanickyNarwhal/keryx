import unittest

import keryx.constants as constants


class TestConstants(unittest.TestCase):
    def test_get_runmode(self):
        self.assertEqual('script', constants.get_runmode())