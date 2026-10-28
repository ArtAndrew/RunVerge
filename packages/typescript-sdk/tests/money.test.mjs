// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { test } from "node:test";
import { parseMicroUSD } from "../dist/index.js";

test("exact int64 financial facts never pass through Number", () => {
  for (const [wire, value] of [["0", 0n], ["1", 1n], ["9007199254740993", 9007199254740993n], ["9223372036854775807", 9223372036854775807n]]) {
    assert.equal(parseMicroUSD(wire), value);
  }
  assert.equal(parseMicroUSD("-9223372036854775808", true), -9223372036854775808n);
  assert.equal(parseMicroUSD("-1", true), -1n);
});

test("noncanonical, numeric and out-of-range money are rejected", () => {
  for (const wire of ["", "-0", "+1", "01", "1.0", "1e6", " 1", "1 ", "1\n", "١", "１", "9223372036854775808", "-9223372036854775809", "9".repeat(10000), 0, 9007199254740993, 1n, true, null, undefined]) {
    for (const signed of [false, true]) assert.throws(() => parseMicroUSD(wire, signed));
  }
  assert.throws(() => parseMicroUSD("-1"));
});
