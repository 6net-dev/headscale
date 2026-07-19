defmodule Registrar.NetTest do
  use ExUnit.Case, async: true

  alias Registrar.Net

  describe "org_prefix/1" do
    test "index 0 is the first /56 of the base /48" do
      assert {:ok, "fd7a:115c:a1e0::/56"} = Net.org_prefix(0)
    end

    test "index selects bits 48..55" do
      assert {:ok, "fd7a:115c:a1e0:ab00::/56"} = Net.org_prefix(0xAB)
      assert {:ok, "fd7a:115c:a1e0:ff00::/56"} = Net.org_prefix(255)
    end

    test "rejects out-of-range indexes" do
      assert {:error, _} = Net.org_prefix(256)
      assert {:error, _} = Net.org_prefix(-1)
    end
  end

  describe "device_address/2" do
    test "places host index in the low word of the org /56" do
      assert {:ok, "fd7a:115c:a1e0:ab00::1"} = Net.device_address(0xAB, 1)
      assert {:ok, "fd7a:115c:a1e0::2a"} = Net.device_address(0, 42)
    end

    test "host index zero is reserved" do
      assert {:error, _} = Net.device_address(0, 0)
    end

    test "bounds are enforced" do
      assert {:ok, _} = Net.device_address(0, 0xFFFF)
      assert {:error, _} = Net.device_address(0, 0x1_0000)
      assert {:error, _} = Net.device_address(256, 1)
    end
  end
end
