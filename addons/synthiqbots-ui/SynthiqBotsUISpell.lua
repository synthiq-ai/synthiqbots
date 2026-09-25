SynthiqBotsUI.getSpellID = function(pInfo)
	local tInfo = SynthiqBotsUI.doSplit(pInfo, "|")
	if(string.sub(tInfo[3], 1, 6) == "Hspell") then return string.sub(tInfo[3], 8) end
	return 0
end

SynthiqBotsUI.addSpell = function(pInfo, pName)
	local tInfo = SynthiqBotsUI.doSplit(pInfo, "|")
	local tID = SynthiqBotsUI.getSpellID(pInfo)
	if(tID == 0) then return end
	
	local tName, tRank, tIcon = GetSpellInfo(tID)
	local tLink = GetSpellLink(tID)
	
	if(tName == nil) then tName = "" end
	if(tRank == nil) then tRank = "" end
	if(tIcon == nil) then tIcon = "inv_misc_questionmark" end
	if(tLink == nil) then tLink = tName end
	
	local tSpell = { tID, tName, tRank, tIcon, tLink }
	
	table.insert(SynthiqBotsUI.spellbook.spells, tSpell)
	SynthiqBotsUI.spellbook.index = SynthiqBotsUI.spellbook.index + 1
	
	if(SynthiqBotsUI.spells[pName] == nil) then SynthiqBotsUI.spells[pName] = {} end
	if(SynthiqBotsUI.spells[pName][tID] == nil) then SynthiqBotsUI.spells[pName][tID] = true end
	
	if(SynthiqBotsUI.spellbook.index < 17) then
		SynthiqBotsUI.setSpell(SynthiqBotsUI.spellbook.index, tSpell, pName)
	end
end

SynthiqBotsUI.setSpell = function(pIndex, pSpell, pName)
	local tIndex = SynthiqBotsUI.IF(pIndex < 10, "0", "") .. pIndex
	local tOverlay = SynthiqBotsUI.spellbook.frames["Overlay"]
	
	if(pSpell ~= nil) then
		local tTitle = SynthiqBotsUI.IF(string.len(pSpell[2]) > 16, string.sub(pSpell[2], 1, 16) .. "...", pSpell[2])
		tOverlay.setButton("S" .. tIndex, pSpell[4], pSpell[5])
		tOverlay.setText("T" .. tIndex, "|cffffcc00" .. tTitle .. "|r")
		tOverlay.setText("R" .. tIndex, "|cff402000" .. pSpell[3] .. "|r")
		tOverlay.buttons["S" .. tIndex].spell = pSpell[1]
		tOverlay.buttons["C" .. tIndex].spell = pSpell[1]
		tOverlay.buttons["S" .. tIndex].doShow()
		tOverlay.buttons["C" .. tIndex].doShow()
		tOverlay.texts["T" .. tIndex]:Show()
		tOverlay.texts["R" .. tIndex]:Show()
		
		tOverlay.buttons["C" .. tIndex]:SetChecked(SynthiqBotsUI.spells[pName][pSpell[1]])
		tOverlay.buttons["C" .. tIndex].doClick = function(pButton)
			local tName = pButton.getName()
			local tAction = ""
			
			SynthiqBotsUI.spells[tName][pButton.spell] = SynthiqBotsUI.IF(SynthiqBotsUI.spells[tName][pButton.spell], false, true)
			pButton:SetChecked(SynthiqBotsUI.spells[tName][pButton.spell])
			
			for id, state in pairs(SynthiqBotsUI.spells[tName]) do
				if(state == false) then tAction = tAction .. SynthiqBotsUI.IF(tAction == "", "ss +", ", +") .. id end
			end
			
			SynthiqBotsUI.ActionToTarget(SynthiqBotsUI.IF(tAction == "", "ss -" .. pButton.spell, tAction), tName)
		end
	else
		tOverlay.buttons["S" .. tIndex].spell = 0
		tOverlay.buttons["C" .. tIndex].spell = 0
		tOverlay.buttons["S" .. tIndex].doHide()
		tOverlay.buttons["C" .. tIndex].doHide()
		tOverlay.texts["T" .. tIndex]:Hide()
		tOverlay.texts["R" .. tIndex]:Hide()
	end
end