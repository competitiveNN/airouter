## Analysis of Model Chain Depth Routing

### Current Behavior
1. **Chain Construction**: The `build_chain` function in `regenerate_config.py` (lines 706-813) constructs fallback chains by preserving the order of models as they appear in the input `models` list. It does **not** sort models by intelligence score.

2. **Model Sorting**: The `sort_models` function (lines 333-385) sorts models by intelligence score descending, but this sorting happens **before** chain construction for each profile (smart, work, fast, large, systemone). The sorted models are then used to build chains that preserve their sorted order.

3. **Session Routing**: The `SelectEndpointForTokens` function (lines 1095-1268) walks chains in the order they appear in `chainOrderLocked` (lines 1419-1458), which uses the order established during chain construction.

### Key Issue
- **Sessions can settle at different depths** in the chain because:
  - The initial rotation (`selectInitialEndpoint`) spreads new sessions across the top of the chain (window of 8 by default)
  - Once a session is pinned to an endpoint, it remains sticky
  - Failover walks **down** the chain (toward cheaper/deeper models) rather than **up** (toward smarter models)
  - A session may fail over multiple times and settle at a lower position in the chain

### Answer to Original Question
**No, the current logic does not guarantee that sessions use at least one of the smartest models.** Sessions can settle at various depths in the chain, potentially using models with lower intelligence scores.

### Suggested Improvements
1. **Intelligence-Aware Chain Ordering**: Modify `build_chain` to ensure the most intelligent models appear earlier in each chain, not just at position 0.

2. **Intelligence-Based Session Assignment**: Instead of random rotation across positions 0-7, assign sessions to endpoints based on intelligence ranking within the chain.

3. **Intelligence-Based Failover Priority**: Modify failover logic to prefer moving up to smarter models when available, rather than always walking down.

4. **Dynamic Chain Reconstruction**: Consider rebuilding chains more frequently to adapt to changing model availability and intelligence distribution.

### Relevant Code Locations
- `regenerate_config.py`: Chain construction (`build_chain` function)
- `router.go`: Session selection and failover logic (`SelectEndpointForTokens`, `chainOrderLocked`)
- `config.go`: Model configuration and preferences

The core issue is that chain construction preserves input order rather than sorting by intelligence, and session routing logic doesn't compensate for this by intelligently selecting endpoints based on model capability.