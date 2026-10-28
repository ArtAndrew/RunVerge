# SPDX-License-Identifier: Elastic-2.0
"""Include release preparation checks in the ordinary development-tools gate."""
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / 'scripts'))
from test_prepare_alpha_release import MetadataFiles, NativeCandidate  # noqa: F401,E402
