/**
 * Completion handling module.
 * Extracts agent responses and builds Slack messages.
 */

export { extractAgentResponse, SUMMARY_TOOL_NAMES } from "./extractor";
export { buildCompletionBlocks, getFallbackText } from "./blocks";
