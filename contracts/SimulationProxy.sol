// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0
pragma solidity 0.8.26;

interface IERC20 {
    function balanceOf(address account) external view returns (uint256);
}

/// @notice Measures what a swap transaction delivers to its sender. It is never
/// deployed: an eth_call places its runtime bytecode at the taker's address with a
/// state override and calls it from the taker, so the swap runs with
/// msg.sender == tx.origin == taker, exactly as when the taker's wallet sends it.
///
/// Build: contracts/build.sh (solc 0.8.26, optimizer 200 runs, evmVersion paris:
/// no PUSH0, so it also runs on chains that are not Shanghai-ready). The runtime
/// bytecode is internal/sim/proxy.go's proxyCode; TestProxyBytecode checks the two
/// match when contracts/SimulationProxy.bin-runtime is present.
contract SimulationProxy {
    address private constant NATIVE = 0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEeE;

    /// @notice Calls `target` with `data` and the call's own value, and reports how
    /// much `tokenOut` this address received, the gas the call used and what it
    /// returned. Does not revert when the swap reverts: `success` is false then and
    /// `returnData` is the revert data.
    /// @dev For an ERC-20 `tokenOut`, `amountOut` is the balance after the swap: the
    /// caller reads the balance before with a separate eth_call. Reading it here would
    /// warm the token, its implementation and the balance slot (EIP-2929), and the swap
    /// would then pay less gas for them than in a real transaction. Reading this
    /// address's own native balance has no such effect.
    function simulate(address target, address tokenOut, bytes calldata data)
        external
        payable
        returns (bool success, uint256 amountOut, uint256 gasUsed, bytes memory returnData)
    {
        uint256 balanceBefore = tokenOut == NATIVE ? address(this).balance : 0;
        uint256 gasBefore = gasleft();
        (success, returnData) = target.call{value: msg.value}(data);
        gasUsed = gasBefore - gasleft();
        if (success) {
            uint256 balanceAfter = _balanceOfSelf(tokenOut);
            amountOut = balanceAfter > balanceBefore ? balanceAfter - balanceBefore : 0;
        }
    }

    function _balanceOfSelf(address token) private view returns (uint256) {
        if (token == NATIVE) {
            return address(this).balance;
        }
        return IERC20(token).balanceOf(address(this));
    }

    /// @dev Routers that pay out the native coin, also with `transfer`'s 2300 gas.
    receive() external payable {}
}
