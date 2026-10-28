// SPDX-License-Identifier: Apache-2.0
export class AgentPlatformError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options);
    this.name = new.target.name;
  }
}

export class APIError extends AgentPlatformError {
  readonly statusCode: number;
  readonly code: string;
  readonly requestId: string | undefined;
  readonly retryable: boolean;

  constructor(options: {
    statusCode: number;
    code: string;
    message: string;
    requestId?: string | undefined;
    retryable?: boolean;
  }) {
    super(options.message);
    this.statusCode = options.statusCode;
    this.code = options.code;
    this.requestId = options.requestId;
    this.retryable = options.retryable === true;
  }
}

export class ProtocolError extends AgentPlatformError {}

export class TransportError extends AgentPlatformError {
  readonly reason: "network" | "timeout" | "aborted";

  constructor(reason: "network" | "timeout" | "aborted", options?: ErrorOptions) {
    super("Unable to complete the API request; the result of a mutation may be unknown", options);
    this.reason = reason;
  }
}
