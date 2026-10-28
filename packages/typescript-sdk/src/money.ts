// SPDX-License-Identifier: Apache-2.0
import type { Money } from "./types.js";

/** Parse exact contract 0.3 micro-USD. Negatives require signed=true for deltas.
 * JSON numbers, whitespace, fractions, exponents, leading zeros and -0 fail.
 * Never converts through Number; callers retain the exact bigint result.
 */
export function parseMicroUSD(value: Money, signed = false): bigint {
  if (typeof value !== "string") throw new TypeError("micro-USD must be a canonical decimal string");
  if (value.length > 20 || !/^(?:0|-?[1-9][0-9]*)(?![\s\S])/.test(value)) {
    throw new TypeError("micro-USD must be a canonical decimal string");
  }
  const amount = BigInt(value);
  if (amount > 9223372036854775807n || amount < (signed ? -9223372036854775808n : 0n)) {
    throw new RangeError("micro-USD is outside the permitted int64 range");
  }
  return amount;
}
