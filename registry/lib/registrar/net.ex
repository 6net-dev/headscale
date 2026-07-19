defmodule Registrar.Net do
  @moduledoc """
  Pure IPv6 arithmetic for the overlay address plan.

  The registry owns `fd7a:115c:a1e0::/48` — the range stock Tailscale
  clients are defined on. Each org takes the /56 selected by its
  `prefix_index` (bits 48..55 of the address); each grant takes the
  host selected by its `host_index` in the low word of the org's /56.
  Host index 0 is reserved (the org's anycast/service slot, unused in
  v1).
  """

  import Bitwise

  @base {0xFD7A, 0x115C, 0xA1E0, 0, 0, 0, 0, 0}
  @max_prefix_index 255
  @max_host_index 0xFFFF

  def base_prefix, do: "fd7a:115c:a1e0::/48"

  def org_prefix(index) when is_integer(index) and index in 0..@max_prefix_index do
    {:ok, format(put_elem(@base, 3, index <<< 8)) <> "/56"}
  end

  def org_prefix(_), do: {:error, :prefix_index_out_of_range}

  def device_address(prefix_index, host_index)
      when is_integer(prefix_index) and prefix_index in 0..@max_prefix_index and
             is_integer(host_index) and host_index in 1..@max_host_index do
    addr =
      @base
      |> put_elem(3, prefix_index <<< 8)
      |> put_elem(7, host_index)

    {:ok, format(addr)}
  end

  def device_address(_, _), do: {:error, :out_of_range}

  defp format(tuple), do: tuple |> :inet.ntoa() |> to_string()
end
